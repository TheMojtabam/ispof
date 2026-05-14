#!/usr/bin/env bash
# install.sh — Ispof installer / updater
#
# What this script does:
#   1. installs the Ispof admin panel binary + systemd unit
#   2. AUTO-DISCOVERS the underlying quiccochet tunnel binary:
#        a. checks PATH
#        b. checks ~10 common bin directories
#        c. scans the whole filesystem (timeout 60s, single-fs only)
#        d. looks for go.mod source trees and builds them if Go is present
#        e. downloads the binary from GitHub with mirror fallback (for
#           network-restricted regions like IR / CN)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/TheMojtabam/ispof/main/scripts/install.sh | sudo bash -s install
#   sudo bash install.sh update
#   sudo bash install.sh uninstall
#   sudo bash install.sh status
#   sudo bash install.sh find-quiccochet     # just report where quiccochet is
#
# Env overrides:
#   ISPOF_VERSION              pin a release tag (default: latest)
#   ISPOF_LISTEN               bind address (default: 0.0.0.0:3000)
#   ISPOF_AUTH                 basic auth (user:pass) — auto-generated if blank
#   ISPOF_OFFLINE              path to local Ispof tarball (air-gapped install)
#   ISPOF_NO_AUTOSTART         don't enable/start the service
#   ISPOF_SKIP_QUICCOCHET      skip the tunnel-binary auto-discovery step
#   QUICCOCHET_REPO            override GitHub repo (default: pechenyeru/quiccochet)
#   QUICCOCHET_OFFLINE         path to local quiccochet binary or source tree
#   QUICCOCHET_PATH            skip discovery, use this binary path directly
#   GITHUB_MIRRORS             colon-separated list of GitHub mirror bases
#                              (default includes ghproxy.com etc. for IR/CN)

set -Eeuo pipefail
IFS=$'\n\t'

# ─────────────────────────── repos & paths ───────────────────────────

REPO="TheMojtabam/ispof"
QUICCOCHET_REPO=${QUICCOCHET_REPO:-"pechenyeru/quiccochet"}
BIN_NAME="ispof"
INSTALL_DIR="/usr/local/bin"
ETC_DIR="/etc/ispof"
SYSTEMD_DIR="/etc/systemd/system"
DEFAULT_FILE="/etc/default/ispof"
LOG_TAG="[ispof-installer]"

# Canonical paths the systemd unit expects.
QUICCOCHET_PATH_FINAL="/usr/local/bin/quiccochet"

# GitHub mirrors for users in network-restricted regions. We try direct
# github.com first; if that fails (timeout/refused) we walk this list.
# Users can override via GITHUB_MIRRORS=base1:base2:base3 — values must
# include the scheme.
DEFAULT_GH_MIRRORS=(
  "https://github.com"
  "https://ghproxy.com/https://github.com"
  "https://gh-proxy.com/https://github.com"
  "https://gh.api.99988866.xyz/https://github.com"
  "https://mirror.ghproxy.com/https://github.com"
)
if [[ -n "${GITHUB_MIRRORS:-}" ]]; then
  IFS=':' read -ra GH_MIRRORS <<< "${GITHUB_MIRRORS}"
else
  GH_MIRRORS=("${DEFAULT_GH_MIRRORS[@]}")
fi

# ─────────────────────────── ui helpers ───────────────────────────

if [[ -t 1 ]]; then
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'
  C_BLUE=$'\033[34m'; C_GREY=$'\033[90m'; C_BOLD=$'\033[1m'; C_OFF=$'\033[0m'
else
  C_GREEN=""; C_YELLOW=""; C_RED=""; C_BLUE=""; C_GREY=""; C_BOLD=""; C_OFF=""
fi

info()  { printf "%s%s%s %s\n" "$C_BLUE"  "$LOG_TAG" "$C_OFF" "$*"; }
ok()    { printf "%s%s%s %s\n" "$C_GREEN" "$LOG_TAG" "$C_OFF" "$*"; }
warn()  { printf "%s%s%s %s\n" "$C_YELLOW" "$LOG_TAG" "$C_OFF" "$*" >&2; }
fail()  { printf "%s%s%s %s\n" "$C_RED"   "$LOG_TAG" "$C_OFF" "$*" >&2; exit 1; }

require_root() {
  [[ $EUID -eq 0 ]] || fail "must run as root (try: sudo $0 $*)"
}

# ─────────────────────────── arch detection ───────────────────────────

detect_arch() {
  local m
  m=$(uname -m)
  case "$m" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    armv7l|armv7)  echo "armv7" ;;
    *) fail "unsupported arch: $m" ;;
  esac
}

# ─────────────────────────── network helpers ───────────────────────────

have() { command -v "$1" >/dev/null 2>&1; }

# `fetch URL OUTFILE [TIMEOUT]` — wraps curl/wget for portability.
# Returns 0 on success, non-zero on any failure (including HTTP errors).
fetch() {
  local url=$1 out=$2 t=${3:-300}
  if have curl; then
    curl -fsSL --connect-timeout 12 --max-time "$t" -o "$out" "$url"
  elif have wget; then
    wget -q --timeout="$t" -O "$out" "$url"
  else
    fail "need curl or wget to download from $url"
  fi
}

# fetch_gh PATH OUTFILE — fetches a github.com path, trying mirrors
# in order if direct GitHub fails. PATH is the part AFTER github.com/,
# e.g. "TheMojtabam/ispof/releases/download/v0.1.0/foo.tar.gz".
fetch_gh() {
  local path=$1 out=$2
  local base url
  for base in "${GH_MIRRORS[@]}"; do
    url="${base}/${path}"
    if fetch "$url" "$out" 2>/dev/null; then
      [[ "$base" != "https://github.com" ]] && ok "  downloaded via $(echo "$base" | sed 's|https://||' | sed 's|/.*||')"
      return 0
    fi
  done
  return 1
}

# can_reach_internet — 0 if any GitHub mirror is reachable, 1 otherwise.
can_reach_internet() {
  local base
  for base in "${GH_MIRRORS[@]}"; do
    if curl -fsSL --connect-timeout 5 --max-time 8 -o /dev/null "$base/" 2>/dev/null; then
      return 0
    fi
  done
  return 1
}

# ─────────────────────────── quiccochet discovery ───────────────────────────

# verify_binary PATH — quick sanity check that a file is an executable Linux binary.
verify_binary() {
  local p=$1
  [[ -f "$p" && -x "$p" ]] || return 1
  if have file; then
    file "$p" 2>/dev/null | grep -qE 'ELF|executable' || return 1
  fi
  return 0
}

# find_quiccochet_binary — searches the whole server. Echoes the path
# to the first matching binary found, returns 1 if nothing.
#
# Strategy:
#   1. PATH         (instant)
#   2. common dirs  (instant — explicit list)
#   3. filesystem   (slow, 60s timeout, single-fs only)
find_quiccochet_binary() {
  # 1. PATH
  if have quiccochet; then
    command -v quiccochet
    return 0
  fi

  # 2. Common locations
  local candidates=(
    /usr/local/bin/quiccochet
    /usr/local/sbin/quiccochet
    /usr/bin/quiccochet
    /usr/sbin/quiccochet
    /opt/quiccochet/quiccochet
    /opt/quiccochet/bin/quiccochet
    /opt/bin/quiccochet
    /root/quiccochet
    /root/bin/quiccochet
    /srv/quiccochet/quiccochet
    /var/lib/quiccochet/quiccochet
    /usr/local/quiccochet/quiccochet
  )
  # Add per-user bin directories
  for home in /home/*; do
    [[ -d "$home" ]] || continue
    candidates+=(
      "$home/quiccochet"
      "$home/bin/quiccochet"
      "$home/.local/bin/quiccochet"
      "$home/go/bin/quiccochet"
    )
  done
  local c
  for c in "${candidates[@]}"; do
    if verify_binary "$c"; then
      echo "$c"
      return 0
    fi
  done

  # 3. Filesystem scan — bounded with timeout to prevent multi-minute hangs
  # on large file servers. We stay on the root filesystem (-xdev) and skip
  # virtual/transient filesystems.
  info "  scanning filesystem (60s budget)..." >&2
  local found
  found=$(timeout 60 find / -xdev -type f \
            \( -name 'quiccochet' -o -name 'quiccochet-linux-*' \) \
            2>/dev/null \
          | grep -v -E '^/(proc|sys|run|dev|var/lib/docker|var/lib/containerd)/' \
          | head -20 || true)
  if [[ -n "$found" ]]; then
    while IFS= read -r f; do
      if verify_binary "$f"; then
        echo "$f"
        return 0
      fi
    done <<< "$found"
  fi

  return 1
}

# find_quiccochet_source — looks for a quiccochet source tree (go.mod
# containing "quiccochet"). Echoes the directory if found.
find_quiccochet_source() {
  info "  scanning for quiccochet source tree (60s budget)..." >&2
  local gomods f
  gomods=$(timeout 60 find / -xdev -type f -name 'go.mod' 2>/dev/null \
            | grep -v -E '^/(proc|sys|run|dev|var/lib/docker|var/lib/containerd)/' \
            | head -100 || true)
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    if grep -qE '^module[[:space:]]+.*quiccochet' "$f" 2>/dev/null; then
      dirname "$f"
      return 0
    fi
  done <<< "$gomods"
  return 1
}

# build_quiccochet_from_source SRC — runs `go build` and installs the
# result to QUICCOCHET_PATH_FINAL. Returns 0 on success, 1 on any failure
# (missing Go, build failure, no main package, ...).
build_quiccochet_from_source() {
  local src=$1
  if ! have go; then
    warn "found source at $src but Go is not installed — install Go ≥ 1.21 to build"
    return 1
  fi
  info "  building from $src"

  local tmp_bin="/tmp/quiccochet.build.$$"
  local out
  # Try a few common module layouts in order: cmd/quiccochet, top-level,
  # any cmd/ subdir, anything with main.go.
  local targets=(
    "./cmd/quiccochet"
    "."
  )
  if [[ -d "$src/cmd" ]]; then
    for d in "$src"/cmd/*/; do
      [[ -d "$d" ]] && targets+=("./cmd/$(basename "$d")")
    done
  fi

  local t
  for t in "${targets[@]}"; do
    out=$( cd "$src" && go build -trimpath -o "$tmp_bin" "$t" 2>&1 ) || true
    if [[ -x "$tmp_bin" ]]; then
      install -m 0755 "$tmp_bin" "$QUICCOCHET_PATH_FINAL"
      rm -f "$tmp_bin"
      ok "  built quiccochet → $QUICCOCHET_PATH_FINAL"
      return 0
    fi
  done

  warn "  build failed: $out"
  rm -f "$tmp_bin"
  return 1
}

# download_quiccochet_binary ARCH — fetches the prebuilt binary from
# GitHub Releases, trying multiple URL patterns and all mirrors.
download_quiccochet_binary() {
  local arch=$1
  local tmp="/tmp/quiccochet.dl.$$"

  # We try several filename conventions because we don't control the
  # release pipeline of the quiccochet repo and naming varies.
  local candidates=(
    "${QUICCOCHET_REPO}/releases/latest/download/quiccochet-linux-${arch}"
    "${QUICCOCHET_REPO}/releases/latest/download/quiccochet-linux-${arch}.tar.gz"
    "${QUICCOCHET_REPO}/releases/latest/download/quiccochet_linux_${arch}.tar.gz"
    "${QUICCOCHET_REPO}/releases/latest/download/quiccochet-${arch}"
    "${QUICCOCHET_REPO}/releases/latest/download/quiccochet"
  )

  local path
  for path in "${candidates[@]}"; do
    info "  trying: $(basename "$path")"
    if fetch_gh "$path" "$tmp" 2>/dev/null; then
      if [[ ! -s "$tmp" ]]; then
        rm -f "$tmp"
        continue
      fi
      # tarball or naked binary?
      if file "$tmp" 2>/dev/null | grep -qE 'gzip|tar'; then
        local extract_dir
        extract_dir=$(mktemp -d)
        if tar -xzf "$tmp" -C "$extract_dir" 2>/dev/null; then
          local extracted
          extracted=$(find "$extract_dir" -type f \( -name 'quiccochet' -o -name 'quiccochet-*' \) -executable | head -1)
          [[ -z "$extracted" ]] && extracted=$(find "$extract_dir" -type f | head -1)
          if [[ -n "$extracted" ]] && verify_binary "$extracted"; then
            install -m 0755 "$extracted" "$QUICCOCHET_PATH_FINAL"
            rm -rf "$extract_dir" "$tmp"
            ok "  installed → $QUICCOCHET_PATH_FINAL"
            return 0
          fi
        fi
        rm -rf "$extract_dir"
      elif verify_binary "$tmp"; then
        install -m 0755 "$tmp" "$QUICCOCHET_PATH_FINAL"
        rm -f "$tmp"
        ok "  installed → $QUICCOCHET_PATH_FINAL"
        return 0
      fi
      rm -f "$tmp"
    fi
  done

  # Last resort: clone the repo and build it ourselves.
  if have git && have go; then
    info "  binary release not found — trying git clone + build"
    local clone_dir="/tmp/quiccochet.src.$$"
    rm -rf "$clone_dir"
    local base
    for base in "${GH_MIRRORS[@]}"; do
      if git clone --depth 1 "${base}/${QUICCOCHET_REPO}" "$clone_dir" 2>/dev/null; then
        if build_quiccochet_from_source "$clone_dir"; then
          rm -rf "$clone_dir"
          return 0
        fi
        rm -rf "$clone_dir"
      fi
    done
  fi

  return 1
}

# ensure_quiccochet — the orchestrator that runs through every option
# in priority order until one succeeds. Returns 0 if quiccochet ends up
# at QUICCOCHET_PATH_FINAL, non-zero otherwise.
ensure_quiccochet() {
  if [[ -n "${ISPOF_SKIP_QUICCOCHET:-}" ]]; then
    info "skipping quiccochet auto-discovery (ISPOF_SKIP_QUICCOCHET set)"
    return 0
  fi

  info "looking for quiccochet binary (control plane needs this to start tunnels)..."

  # 0. User-specified path wins.
  if [[ -n "${QUICCOCHET_PATH:-}" ]]; then
    if verify_binary "$QUICCOCHET_PATH"; then
      ok "  using user-specified: $QUICCOCHET_PATH"
      [[ "$QUICCOCHET_PATH" != "$QUICCOCHET_PATH_FINAL" ]] \
        && install -m 0755 "$QUICCOCHET_PATH" "$QUICCOCHET_PATH_FINAL"
      return 0
    fi
    fail "QUICCOCHET_PATH=$QUICCOCHET_PATH is not a valid executable"
  fi

  # 0.5. Offline bundle wins next.
  if [[ -n "${QUICCOCHET_OFFLINE:-}" ]]; then
    if [[ -d "$QUICCOCHET_OFFLINE" ]]; then
      info "  using offline source tree: $QUICCOCHET_OFFLINE"
      build_quiccochet_from_source "$QUICCOCHET_OFFLINE" && return 0
    elif verify_binary "$QUICCOCHET_OFFLINE"; then
      info "  using offline binary: $QUICCOCHET_OFFLINE"
      install -m 0755 "$QUICCOCHET_OFFLINE" "$QUICCOCHET_PATH_FINAL"
      return 0
    fi
    fail "QUICCOCHET_OFFLINE=$QUICCOCHET_OFFLINE is neither a valid binary nor a directory"
  fi

  # 1. Already at canonical location?
  if verify_binary "$QUICCOCHET_PATH_FINAL"; then
    ok "quiccochet already installed at $QUICCOCHET_PATH_FINAL"
    return 0
  fi

  # 2. Search server for an existing binary.
  local existing
  if existing=$(find_quiccochet_binary); then
    ok "found existing quiccochet at: $existing"
    if [[ "$existing" != "$QUICCOCHET_PATH_FINAL" ]]; then
      info "  copying to $QUICCOCHET_PATH_FINAL (so systemd can find it)"
      install -m 0755 "$existing" "$QUICCOCHET_PATH_FINAL"
    fi
    return 0
  fi
  warn "no quiccochet binary anywhere on the server"

  # 3. Search server for source code and try to build.
  local src
  if src=$(find_quiccochet_source); then
    info "found source at $src — attempting build"
    build_quiccochet_from_source "$src" && return 0
    warn "  source build failed"
  fi

  # 4. Online download (with mirror fallback for IR/CN).
  if can_reach_internet; then
    info "downloading quiccochet from github.com/${QUICCOCHET_REPO}"
    download_quiccochet_binary "$(detect_arch)" && return 0
    warn "online download failed (binary may not exist in releases yet)"
  else
    warn "no internet connectivity — cannot download"
  fi

  # 5. Give up gracefully — Ispof itself still works, just no tunnels.
  warn "could not install quiccochet automatically. The Ispof panel is"
  warn "still installed and will start, but you cannot start tunnels"
  warn "until quiccochet is available at $QUICCOCHET_PATH_FINAL."
  warn ""
  warn "to install manually:"
  warn "  1. download from https://github.com/${QUICCOCHET_REPO}/releases"
  warn "  2. or build from source: git clone, then 'go build'"
  warn "  3. or run: sudo bash $0 install-quiccochet"
  return 1
}

# ─────────────────────────── ispof itself ───────────────────────────

ensure_user() {
  if ! id ispof >/dev/null 2>&1; then
    info "creating system user 'ispof'"
    useradd --system --no-create-home --shell /usr/sbin/nologin ispof
  fi
}

install_ispof_binary() {
  local version=$1 arch=$2 tarball workdir

  if [[ -n "${ISPOF_OFFLINE:-}" ]]; then
    [[ -f "$ISPOF_OFFLINE" ]] || fail "ISPOF_OFFLINE='$ISPOF_OFFLINE' does not exist"
    info "using offline tarball: $ISPOF_OFFLINE"
    tarball=$ISPOF_OFFLINE
  else
    local fname="ispof-${version}-linux-${arch}.tar.gz"
    workdir=$(mktemp -d)
    trap 'rm -rf "$workdir"' EXIT
    tarball="$workdir/$fname"

    info "downloading ispof $version"
    if ! fetch_gh "${REPO}/releases/download/${version}/${fname}" "$tarball"; then
      fail "could not download ispof release — check ISPOF_VERSION or network"
    fi

    # SHA256 verification if sidecar is published
    if fetch_gh "${REPO}/releases/download/${version}/${fname}.sha256" "${tarball}.sha256" 2>/dev/null; then
      info "verifying sha256"
      ( cd "$workdir" && sha256sum -c "${fname}.sha256" >/dev/null ) || fail "checksum mismatch"
      ok "checksum verified"
    else
      warn "no .sha256 sidecar published for this release — skipping checksum verification"
    fi
  fi

  info "extracting → $INSTALL_DIR"
  install -d -m 0755 "$INSTALL_DIR"
  tar -xzf "$tarball" -C "$INSTALL_DIR" --strip-components=0 "$BIN_NAME" 2>/dev/null \
    || tar -xzf "$tarball" -C "$INSTALL_DIR" "$BIN_NAME"
  chmod 0755 "$INSTALL_DIR/$BIN_NAME"
}

latest_version() {
  if [[ -n "${ISPOF_VERSION:-}" ]]; then
    echo "$ISPOF_VERSION"
    return
  fi
  # The "latest" tag is published by CI on every main push.
  echo "latest"
}

write_units() {
  local listen=${ISPOF_LISTEN:-0.0.0.0:3000}
  local auth=${ISPOF_AUTH:-}

  install -d -m 0750 "$ETC_DIR" "$ETC_DIR/tunnels" "$ETC_DIR/keys"
  chown -R ispof:ispof "$ETC_DIR"
  chmod 0700 "$ETC_DIR/keys"

  info "writing $DEFAULT_FILE"
  cat > "$DEFAULT_FILE" <<EOF
# Ispof environment file — edit and \`systemctl restart ispof\`.
ISPOF_LISTEN=${listen}
ISPOF_LOG_LEVEL=info
ISPOF_AUTH=${auth}
EOF
  chmod 0640 "$DEFAULT_FILE"

  info "writing systemd units"
  cat > "$SYSTEMD_DIR/ispof.service" <<'EOF'
[Unit]
Description=Ispof Admin Panel
Documentation=https://github.com/TheMojtabam/ispof
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ispof
Group=ispof
EnvironmentFile=-/etc/default/ispof
ExecStart=/usr/local/bin/ispof --listen ${ISPOF_LISTEN} --tunnels-dir /etc/ispof/tunnels --keys-dir /etc/ispof/keys --log-level ${ISPOF_LOG_LEVEL} --auth ${ISPOF_AUTH}
Restart=on-failure
RestartSec=5
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/etc/ispof

[Install]
WantedBy=multi-user.target
EOF

  cat > "$SYSTEMD_DIR/quiccochet@.service" <<'EOF'
[Unit]
Description=QUICochet tunnel: %i
Documentation=https://github.com/TheMojtabam/ispof
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/quiccochet -c /etc/ispof/tunnels/%i.json
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=15
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
}

# ─────────────────────────── commands ───────────────────────────

cmd_install() {
  require_root install
  local arch version
  arch=$(detect_arch)
  version=$(latest_version)

  info "installing ispof $version (linux/$arch)"
  ensure_user
  install_ispof_binary "$version" "$arch"
  write_units

  if [[ -z "${ISPOF_AUTH:-}" ]] && grep -q '^ISPOF_AUTH=$' "$DEFAULT_FILE"; then
    local pw
    pw=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 16 || echo "changeme$(date +%s)")
    sed -i "s|^ISPOF_AUTH=$|ISPOF_AUTH=admin:${pw}|" "$DEFAULT_FILE"
    warn "generated random panel password — saved in $DEFAULT_FILE"
    warn "        username: admin"
    warn "        password: ${pw}"
  fi

  # The big one: auto-discover quiccochet. Non-fatal if it fails — the
  # panel still installs and runs, you just can't start tunnels until
  # you provide the binary.
  ensure_quiccochet || true

  if [[ -z "${ISPOF_NO_AUTOSTART:-}" ]]; then
    info "enabling + starting ispof.service"
    systemctl enable --now ispof.service
    sleep 1
    if systemctl is-active --quiet ispof; then
      ok "ispof is running"
      info "panel URL: http://${ISPOF_LISTEN:-0.0.0.0:3000}/"
    else
      warn "service did not enter active state — check: journalctl -u ispof -n 50"
    fi
  fi

  cat <<EOF

${C_BOLD}done.${C_OFF}

  ${C_GREY}# inspect:${C_OFF}        systemctl status ispof
  ${C_GREY}# logs:${C_OFF}           journalctl -u ispof -f
  ${C_GREY}# panel URL:${C_OFF}      http://${ISPOF_LISTEN:-0.0.0.0:3000}/
  ${C_GREY}# config dir:${C_OFF}     ${ETC_DIR}/tunnels/
  ${C_GREY}# quiccochet:${C_OFF}     $(verify_binary "$QUICCOCHET_PATH_FINAL" && echo "✓ $QUICCOCHET_PATH_FINAL" || echo "✗ not installed — run: $0 install-quiccochet")
  ${C_GREY}# uninstall:${C_OFF}      sudo $0 uninstall

EOF
}

# install-quiccochet — standalone command for installing just the tunnel
# binary (useful if the panel was already installed but the auto-discovery
# couldn't find quiccochet at install time).
cmd_install_quiccochet() {
  require_root install-quiccochet
  ensure_quiccochet
  if verify_binary "$QUICCOCHET_PATH_FINAL"; then
    ok "quiccochet is ready at $QUICCOCHET_PATH_FINAL"
  else
    fail "could not install quiccochet — see warnings above"
  fi
}

# find-quiccochet — just report where quiccochet is (or isn't) without
# changing anything. Useful for debugging.
cmd_find_quiccochet() {
  info "looking for quiccochet..."
  local existing
  if existing=$(find_quiccochet_binary); then
    ok "binary: $existing"
  else
    warn "no binary found"
  fi
  local src
  if src=$(find_quiccochet_source); then
    ok "source: $src"
  else
    warn "no source tree found"
  fi
  if can_reach_internet; then
    ok "network: github reachable directly or via mirrors"
  else
    warn "network: github unreachable"
  fi
}

cmd_update() {
  require_root update
  local arch version current
  arch=$(detect_arch)
  version=$(latest_version)
  if [[ -x "$INSTALL_DIR/$BIN_NAME" ]]; then
    current=$("$INSTALL_DIR/$BIN_NAME" -version 2>/dev/null | awk '{print $2}' || echo "unknown")
    info "current: $current → target: $version"
  fi
  install_ispof_binary "$version" "$arch"
  systemctl daemon-reload
  systemctl restart ispof || warn "restart failed — service may not be installed"
  ok "updated to $version"
}

cmd_uninstall() {
  require_root uninstall
  warn "this will remove ispof and its systemd units"
  read -rp "continue? [y/N] " ans
  [[ "${ans:-}" =~ ^[Yy]$ ]] || { info "aborted"; exit 0; }

  for unit in $(systemctl list-units --type=service --all --no-legend | awk '/^quiccochet@/ {print $1}'); do
    info "stopping $unit"
    systemctl stop "$unit" || true
    systemctl disable "$unit" || true
  done

  systemctl stop ispof.service 2>/dev/null || true
  systemctl disable ispof.service 2>/dev/null || true

  rm -f "$SYSTEMD_DIR/ispof.service" "$SYSTEMD_DIR/quiccochet@.service"
  rm -f "$INSTALL_DIR/$BIN_NAME"
  rm -f "$DEFAULT_FILE"
  systemctl daemon-reload

  read -rp "remove /etc/ispof (tunnel configs + keys)? [y/N] " ans2
  if [[ "${ans2:-}" =~ ^[Yy]$ ]]; then
    rm -rf "$ETC_DIR"
    ok "removed $ETC_DIR"
  else
    info "kept $ETC_DIR (re-install will reuse existing configs)"
  fi

  read -rp "also remove $QUICCOCHET_PATH_FINAL? [y/N] " ans3
  if [[ "${ans3:-}" =~ ^[Yy]$ ]]; then
    rm -f "$QUICCOCHET_PATH_FINAL"
  fi

  userdel ispof 2>/dev/null || true
  ok "uninstall complete"
}

cmd_status() {
  if ! [[ -x "$INSTALL_DIR/$BIN_NAME" ]]; then
    warn "ispof not installed"
    exit 1
  fi
  "$INSTALL_DIR/$BIN_NAME" -version || true
  echo
  echo "${C_BOLD}panel:${C_OFF}"
  systemctl status ispof.service --no-pager -l 2>&1 | head -20 || true
  echo
  echo "${C_BOLD}quiccochet:${C_OFF}"
  if verify_binary "$QUICCOCHET_PATH_FINAL"; then
    echo "  ✓ $QUICCOCHET_PATH_FINAL"
  else
    echo "  ✗ not installed at $QUICCOCHET_PATH_FINAL"
  fi
  echo
  echo "${C_BOLD}tunnel units:${C_OFF}"
  systemctl list-units --type=service --all --no-legend | awk '/^quiccochet@/ {print "  " $0}' || echo "  (none)"
}

# ─────────────────────────── main ───────────────────────────

main() {
  local cmd=${1:-install}
  case "$cmd" in
    install)            cmd_install ;;
    install-quiccochet) cmd_install_quiccochet ;;
    find-quiccochet)    cmd_find_quiccochet ;;
    update)             cmd_update ;;
    uninstall)          cmd_uninstall ;;
    status)             cmd_status ;;
    -h|--help|help)
      cat <<EOF
Ispof installer

Usage: $0 {install|update|uninstall|status|install-quiccochet|find-quiccochet}

Env:
  ISPOF_VERSION         pin a release tag (default: latest)
  ISPOF_LISTEN          bind address (default: 0.0.0.0:3000)
  ISPOF_AUTH            basic auth (user:pass) — auto-generated if blank
  ISPOF_OFFLINE         path to local Ispof tarball (air-gapped)
  ISPOF_NO_AUTOSTART    don't enable/start the service
  ISPOF_SKIP_QUICCOCHET skip the tunnel-binary auto-discovery step

  QUICCOCHET_REPO       override repo (default: pechenyeru/quiccochet)
  QUICCOCHET_PATH       skip discovery, use this binary directly
  QUICCOCHET_OFFLINE    path to local quiccochet binary or source tree
  GITHUB_MIRRORS        colon-separated GitHub mirror bases (for IR/CN)
EOF
      ;;
    *) fail "unknown command: $cmd (try: install, update, uninstall, status, install-quiccochet, find-quiccochet)" ;;
  esac
}

main "$@"

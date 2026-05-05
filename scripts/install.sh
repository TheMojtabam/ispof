#!/usr/bin/env bash
# ============================================================
# QUICochet — unified installer (tunnel + web panel)
#
# Usage:
#   curl -fsSL <URL>/install.sh | sudo bash -s -- install
#   curl -fsSL <URL>/install.sh | sudo bash -s -- update
#   curl -fsSL <URL>/install.sh | sudo bash -s -- remove
#   curl -fsSL <URL>/install.sh | sudo bash -s -- status
#   curl -fsSL <URL>/install.sh | sudo bash -s -- iran-installer
# ============================================================
set -euo pipefail

NAME="quiccochet"
PANEL="qcc-panel"
GH_REPO="${QCC_REPO:-pechenyeru/quiccochet}"
VERSION="${QCC_VERSION:-latest}"
SIDE="${QCC_SIDE:-server}"     # "server" (foreign) or "client" (iran)

INSTALL_DIR="/usr/local/bin"
TUN_BIN="${INSTALL_DIR}/${NAME}"
PANEL_BIN="${INSTALL_DIR}/${PANEL}"
CTL="${INSTALL_DIR}/qcc-ctl"

CONFIG_DIR="/etc/${NAME}"
DATA_DIR="/var/lib/${NAME}"
LOG_DIR="/var/log/${NAME}"
RUN_DIR="/run/${NAME}"

SVC_TUNNEL="/etc/systemd/system/${NAME}.service"
SVC_PANEL="/etc/systemd/system/${NAME}-panel.service"

PANEL_PORT="${QCC_PANEL_PORT:-9999}"
TUNNEL_PORT="${QCC_TUNNEL_PORT:-8443}"
ADMIN_USER="${QCC_PANEL_USER:-admin}"

R='\033[0;31m'; G='\033[0;32m'; Y='\033[0;33m'; C='\033[0;36m'; B='\033[1m'; N='\033[0m'
log()  { echo -e "${C}[QCC]${N} $*"; }
ok()   { echo -e "${G}[ ✓ ]${N} $*"; }
warn() { echo -e "${Y}[ ! ]${N} $*"; }
err()  { echo -e "${R}[ ✗ ]${N} $*" >&2; }
die()  { err "$*"; exit 1; }

[[ $EUID -eq 0 ]] || die "این اسکریپت رو با sudo اجرا کن"

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "معماری پشتیبانی نمی‌شه: $(uname -m)" ;;
  esac
}
detect_os() {
  if [[ -f /etc/debian_version ]]; then echo "debian"
  elif [[ -f /etc/redhat-release ]]; then echo "rhel"
  elif [[ -f /etc/alpine-release ]]; then echo "alpine"
  else echo "linux"; fi
}

install_deps() {
  log "بررسی پیش‌نیازها..."
  local script_dir="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

  # In offline mode (binaries are next to install.sh), we don't need jq or curl —
  # they're only used to download from GitHub. We only need the bare minimum.
  local required=(tar openssl)
  local online_only=(curl jq)

  local missing=()
  for tool in "${required[@]}"; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done

  if [[ ${#missing[@]} -ne 0 ]]; then
    err "این ابزارهای ضروری گم هستن: ${missing[*]}"
    err "روی Debian/Ubuntu:  apt install -y ${missing[*]}"
    err "روی RHEL/CentOS:    yum install -y ${missing[*]}"
    die "نصب نمی‌تونه ادامه بده"
  fi

  # If binaries are local (offline mode), we're done — no apt-get needed
  if [[ -x "${script_dir}/quiccochet" && -x "${script_dir}/qcc-panel" ]]; then
    ok "حالت آفلاین — همه پیش‌نیازها OK (jq/curl نیاز نیست)"
    return 0
  fi

  # Online mode — also need jq + curl for downloading
  for tool in "${online_only[@]}"; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done

  if [[ ${#missing[@]} -eq 0 ]]; then
    ok "همه پیش‌نیازها از قبل موجوده"
    return 0
  fi

  # Quick internet probe before trying apt
  if ! timeout 2 bash -c 'cat < /dev/null > /dev/tcp/1.1.1.1/443' 2>/dev/null; then
    err "اینترنت در دسترس نیست و این ابزارها گم هستن: ${missing[*]}"
    err "اگه می‌خوای نصب آفلاین کنی:"
    err "  ۱) tarball release رو روی کامپیوتر دیگه دانلود کن"
    err "  ۲) scp کن به این سرور"
    err "  ۳) tar -xzf quiccochet-linux-amd64.tar.gz"
    err "  ۴) cd quiccochet-linux-amd64/"
    err "  ۵) sudo ./install.sh install"
    die "نصب نمی‌تونه ادامه بده"
  fi

  log "نصب پیش‌نیازها (${missing[*]})..."
  case "$(detect_os)" in
    debian)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq
      apt-get install -y -qq curl wget jq tar ca-certificates iproute2 iptables ufw openssl
      ;;
    rhel)
      yum install -y -q curl wget jq tar ca-certificates iproute iptables firewalld openssl
      ;;
    alpine)
      apk add --no-cache curl wget jq tar ca-certificates iproute2 iptables openssl bash
      ;;
  esac
  ok "پیش‌نیازها نصب شد"
}

apply_sysctl() {
  log "اعمال sysctl tuning..."
  cat > /etc/sysctl.d/99-${NAME}.conf <<'EOF'
# QUICochet — high throughput tuning
net.core.rmem_max=134217728
net.core.wmem_max=134217728
net.core.rmem_default=2097152
net.core.wmem_default=2097152
net.core.netdev_max_backlog=30000
net.core.somaxconn=4096
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
net.ipv4.tcp_rmem=4096 87380 67108864
net.ipv4.tcp_wmem=4096 65536 67108864
net.ipv4.tcp_fastopen=3
net.ipv4.tcp_mtu_probing=1
net.ipv4.udp_mem=102400 873800 134217728
net.ipv4.udp_rmem_min=8192
net.ipv4.udp_wmem_min=8192
net.netfilter.nf_conntrack_max=1048576
net.ipv4.ip_forward=1
EOF
  sysctl -p /etc/sysctl.d/99-${NAME}.conf >/dev/null 2>&1 || warn "بعضی sysctl ها اعمال نشد"
  modprobe tcp_bbr 2>/dev/null || true
  ok "sysctl اعمال شد"
}

download_release() {
  local arch=$(detect_arch)
  local tmp=$(mktemp -d)
  local script_dir="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

  # ============================================================
  # OFFLINE MODE — both binaries already next to install.sh
  # This is the path when you scp'd a release tarball to a server
  # with no internet. NO Go, NO git, NO downloads needed.
  # ============================================================
  if [[ -x "${script_dir}/quiccochet" && -x "${script_dir}/qcc-panel" ]]; then
    log "حالت آفلاین — باینری‌های محلی پیدا شد"
    install -m 0755 "${script_dir}/quiccochet" "$TUN_BIN"
    install -m 0755 "${script_dir}/qcc-panel"  "$PANEL_BIN"
    rm -rf "$tmp"
    ok "باینری‌ها از فایل محلی نصب شدن (بدون نیاز به اینترنت)"
    return 0
  fi

  # ============================================================
  # ONLINE MODE — fetch tarball from GitHub release
  # ============================================================
  if [[ "$VERSION" == "latest" ]]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${GH_REPO}/releases/latest" 2>/dev/null \
              | jq -r '.tag_name' 2>/dev/null || echo "")
  fi
  if [[ -z "$VERSION" || "$VERSION" == "null" ]]; then
    err "نتونستم آخرین release رو از GitHub پیدا کنم."
    err ""
    err "اگه این سرور اینترنت نداره، فلوی آفلاین رو دنبال کن:"
    err "  ۱) روی کامپیوتر دیگه: tarball رو از https://github.com/${GH_REPO}/releases دانلود کن"
    err "  ۲) scp کن به این سرور"
    err "  ۳) tar -xzf quiccochet-linux-${arch}.tar.gz"
    err "  ۴) cd quiccochet-linux-${arch}/"
    err "  ۵) sudo ./install.sh install"
    die "نصب نمی‌تونه ادامه بده — release لازمه"
  fi
  local url="https://github.com/${GH_REPO}/releases/download/${VERSION}/quiccochet-linux-${arch}.tar.gz"
  log "دانلود $VERSION از GitHub..."
  if ! curl -fsSL "$url" -o "$tmp/q.tgz" 2>/dev/null; then
    err "دانلود از $url ناموفق بود"
    err "احتمالاً release هنوز ساخته نشده. می‌تونی workflow رو روی GitHub trigger کنی"
    err "یا منتظر بمونی tag-push درست تموم شه."
    die "نصب نمی‌تونه ادامه بده"
  fi
  tar -xzf "$tmp/q.tgz" -C "$tmp"
  # Tarball contains a top-level "quiccochet-linux-amd64/" directory,
  # so look both at root and in any subdir.
  local src_tun="$tmp/quiccochet"
  local src_panel="$tmp/qcc-panel"
  [[ -f "$src_tun" ]] || src_tun=$(find "$tmp" -name quiccochet -type f -executable | head -1)
  [[ -f "$src_panel" ]] || src_panel=$(find "$tmp" -name qcc-panel -type f -executable | head -1)
  install -m 0755 "$src_tun"   "$TUN_BIN"
  install -m 0755 "$src_panel" "$PANEL_BIN"
  rm -rf "$tmp"
  ok "باینری‌ها نصب شدن"
}

generate_config() {
  mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" "$RUN_DIR" "${DATA_DIR}/backups" "${DATA_DIR}/installers"
  chmod 700 "$CONFIG_DIR"

  if [[ -f "$CONFIG_DIR/${SIDE}.json" && -f "$CONFIG_DIR/panel.json" ]]; then
    log "کانفیگ موجوده — رد می‌شیم"
    return
  fi

  log "تولید کلیدها و کانفیگ ($SIDE)..."
  local priv pub token pass pub_ip
  priv=$("$TUN_BIN" keygen 2>/dev/null | grep -i "private:" | awk '{print $NF}')
  pub=$("$TUN_BIN" keygen 2>/dev/null | grep -i "public:" | awk '{print $NF}')
  if [[ -z "$priv" ]]; then
    priv=$("$PANEL_BIN" genkey)
    pub=$(echo "$priv" | "$PANEL_BIN" pubkey)
  fi
  token=$(openssl rand -hex 24)
  pass=$(openssl rand -base64 12 | tr -d '/+=' | head -c 16)
  pub_ip=$(curl -fsSL --max-time 2 https://api.ipify.org 2>/dev/null \
           || curl -fsSL --max-time 2 https://ifconfig.me 2>/dev/null \
           || hostname -I | awk '{print $1}')

  if [[ "$SIDE" == "server" ]]; then
    cat > "$CONFIG_DIR/server.json" <<JSON
{
  "mode": "server",
  "transport": { "type": "udp" },
  "listen_port": ${TUNNEL_PORT},
  "spoof": {
    "source_ip": "10.99.0.1",
    "peer_spoof_ip": "10.99.0.2",
    "client_real_ip": "0.0.0.0"
  },
  "crypto": {
    "private_key": "${priv}",
    "peer_public_key": ""
  },
  "obfuscation": { "enabled": true, "mode": "standard" },
  "security": { "block_private_targets": true },
  "admin": { "socket_path": "${RUN_DIR}/admin.sock" },
  "logging": { "level": "info", "file": "${LOG_DIR}/tunnel.log" }
}
JSON
  else
    cat > "$CONFIG_DIR/client.json" <<JSON
{
  "mode": "client",
  "transport": { "type": "udp" },
  "server": { "address": "REPLACE_WITH_FOREIGN_IP", "port": ${TUNNEL_PORT} },
  "spoof": {
    "source_ip": "10.99.0.2",
    "peer_spoof_ip": "10.99.0.1",
    "client_real_ip": "0.0.0.0"
  },
  "crypto": {
    "private_key": "${priv}",
    "peer_public_key": "REPLACE_WITH_FOREIGN_PUBLIC_KEY"
  },
  "obfuscation": { "enabled": true, "mode": "standard" },
  "admin": { "socket_path": "${RUN_DIR}/admin.sock" },
  "logging": { "level": "info", "file": "${LOG_DIR}/tunnel.log" }
}
JSON
  fi

  cat > "$CONFIG_DIR/panel.json" <<JSON
{
  "side": "${SIDE}",
  "listen": "0.0.0.0:${PANEL_PORT}",
  "admin_user": "${ADMIN_USER}",
  "admin_pass": "${pass}",
  "agent_token": "${token}",
  "data_dir": "${DATA_DIR}",
  "log_dir": "${LOG_DIR}",
  "tunnel_config": "${CONFIG_DIR}/${SIDE}.json",
  "xray_config": "/etc/xray/config.json",
  "rate_limit_logins": true
}
JSON

  chmod 600 "$CONFIG_DIR"/*.json

  cat > "$CONFIG_DIR/.creds" <<EOF
SIDE=${SIDE}
PANEL_URL=http://${pub_ip}:${PANEL_PORT}
PANEL_USER=${ADMIN_USER}
PANEL_PASS=${pass}
AGENT_TOKEN=${token}
PUBLIC_KEY=${pub}
PRIVATE_KEY=${priv}
PUBLIC_IP=${pub_ip}
EOF
  chmod 600 "$CONFIG_DIR/.creds"
  ok "کانفیگ‌ها در $CONFIG_DIR ساخته شد"
}

install_services() {
  cat > "$SVC_TUNNEL" <<UNIT
[Unit]
Description=QUICochet Tunnel Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/bin/mkdir -p ${RUN_DIR}
ExecStart=${TUN_BIN} -c ${CONFIG_DIR}/${SIDE}.json
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_RAW CAP_NET_ADMIN CAP_NET_BIND_SERVICE
StandardOutput=append:${LOG_DIR}/tunnel.log
StandardError=append:${LOG_DIR}/tunnel.err

[Install]
WantedBy=multi-user.target
UNIT

  cat > "$SVC_PANEL" <<UNIT
[Unit]
Description=QUICochet Web Panel
After=network-online.target ${NAME}.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=${PANEL_BIN} panel --config ${CONFIG_DIR}/panel.json
Restart=on-failure
RestartSec=5
StandardOutput=append:${LOG_DIR}/panel.log
StandardError=append:${LOG_DIR}/panel.err

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable ${NAME}.service ${NAME}-panel.service 2>/dev/null || true
  if [[ "$SIDE" == "server" ]] || [[ -f "$CONFIG_DIR/${SIDE}.json" && ! "$(grep REPLACE_WITH "$CONFIG_DIR/${SIDE}.json")" ]]; then
    systemctl start ${NAME}-panel.service 2>/dev/null || true
    [[ "$SIDE" == "server" ]] && systemctl start ${NAME}.service 2>/dev/null || true
  else
    warn "کانفیگ ایران هنوز placeholder داره — بعد از پر کردنش، systemctl start ${NAME} بزن"
  fi
  ok "سرویس‌ها فعال شدن"
}

configure_firewall() {
  if command -v ufw >/dev/null 2>&1; then
    ufw allow ${PANEL_PORT}/tcp >/dev/null 2>&1 || true
    ufw allow ${TUNNEL_PORT}/udp >/dev/null 2>&1 || true
    if [[ "$SIDE" == "client" ]]; then
      ufw allow 443/tcp >/dev/null 2>&1 || true
      ufw allow 80/tcp >/dev/null 2>&1 || true
    fi
    ok "ufw: پورت‌ها باز شدن"
  elif command -v firewall-cmd >/dev/null 2>&1; then
    firewall-cmd --permanent --add-port=${PANEL_PORT}/tcp >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-port=${TUNNEL_PORT}/udp >/dev/null 2>&1 || true
    if [[ "$SIDE" == "client" ]]; then
      firewall-cmd --permanent --add-port=443/tcp >/dev/null 2>&1 || true
    fi
    firewall-cmd --reload >/dev/null 2>&1 || true
    ok "firewalld: پورت‌ها باز شدن"
  fi
}

install_ctl() {
  cat > "$CTL" <<'CTLEOF'
#!/usr/bin/env bash
# qcc-ctl — QUICochet management helper
set -euo pipefail
NAME="quiccochet"
CONFIG_DIR="/etc/${NAME}"
DATA_DIR="/var/lib/${NAME}"
LOG_DIR="/var/log/${NAME}"

case "${1:-}" in
  status)
    for s in ${NAME} ${NAME}-panel; do
      if systemctl is-active --quiet "$s"; then
        echo "  $s: ● active"
      else
        echo "  $s: ○ inactive"
      fi
    done
    [[ -f "$CONFIG_DIR/.creds" ]] && { source "$CONFIG_DIR/.creds"; echo "  panel: $PANEL_URL  user: $PANEL_USER  side: $SIDE"; }
    echo
    if /usr/local/bin/${NAME} admin stats 2>/dev/null; then :; else echo "  (tunnel admin socket not reachable)"; fi
    ;;
  logs)
    shift
    journalctl -u ${NAME} -u ${NAME}-panel -f --no-pager "$@"
    ;;
  restart)  systemctl restart ${NAME} ${NAME}-panel; echo "✓ ری‌استارت شد" ;;
  stop)     systemctl stop    ${NAME} ${NAME}-panel ;;
  start)    systemctl start   ${NAME} ${NAME}-panel ;;
  password-reset)
    new=$(openssl rand -base64 12 | tr -d '/+=')
    /usr/local/bin/qcc-panel hash-password "$new" > /tmp/.qcc-hash 2>/dev/null || echo "$new" > /tmp/.qcc-hash
    HASH=$(cat /tmp/.qcc-hash); rm /tmp/.qcc-hash
    sed -i "s|\"admin_pass\":.*|\"admin_pass\": \"$HASH\",|" "$CONFIG_DIR/panel.json"
    sed -i "s|^PANEL_PASS=.*|PANEL_PASS=$new|" "$CONFIG_DIR/.creds"
    systemctl restart ${NAME}-panel
    echo "✓ پسورد جدید: $new"
    ;;
  backup)
    ts=$(date +%Y%m%d-%H%M%S)
    out="$DATA_DIR/backups/qcc-$ts.tar.gz"
    mkdir -p "$DATA_DIR/backups"
    tar -czf "$out" -C / etc/${NAME} 2>/dev/null
    echo "✓ بک‌آپ: $out"
    ;;
  iran-installer)
    [[ -f "$CONFIG_DIR/.creds" ]] || { echo "creds پیدا نشد"; exit 1; }
    source "$CONFIG_DIR/.creds"
    rnd=$(openssl rand -hex 4)
    out="$DATA_DIR/installers/iran-$rnd.sh"
    mkdir -p "$DATA_DIR/installers"
    sed -e "s|@@FOREIGN_IP@@|$PUBLIC_IP|g" \
        -e "s|@@FOREIGN_PUB@@|$PUBLIC_KEY|g" \
        -e "s|@@AGENT_TOKEN@@|$AGENT_TOKEN|g" \
        -e "s|@@PANEL_URL@@|$PANEL_URL|g" \
        /usr/local/share/quiccochet/iran-template.sh > "$out" 2>/dev/null \
      || echo "ERROR: iran-template.sh missing"
    chmod 755 "$out"
    echo "✓ Iran installer: $out"
    echo
    echo "  scp $out root@<IRAN_IP>:/tmp/"
    echo "  ssh root@<IRAN_IP> 'sudo bash /tmp/$(basename $out)'"
    echo
    echo "  یا از پنل خارج: تب «سرور ایران» → دکمه دانلود installer"
    ;;
  *)
    cat <<HELP
qcc-ctl — مدیریت QUICochet
  status          وضعیت سرویس‌ها + admin stats از تونل زنده
  logs            tail لاگ زنده (هر دو سرویس)
  start           شروع
  stop            توقف
  restart         ری‌استارت
  password-reset  پسورد admin جدید
  backup          پشتیبان‌گیری فوری
  iran-installer  ساخت اسکریپت نصب اختصاصی برای ایران
HELP
    ;;
esac
CTLEOF
  chmod +x "$CTL"

  # iran installer template
  mkdir -p /usr/local/share/quiccochet
  cat > /usr/local/share/quiccochet/iran-template.sh <<'IRAN'
#!/usr/bin/env bash
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "with sudo"; exit 1; }
FOREIGN_IP="@@FOREIGN_IP@@"
FOREIGN_PUB="@@FOREIGN_PUB@@"
AGENT_TOKEN="@@AGENT_TOKEN@@"
PANEL_URL="@@PANEL_URL@@"
QCC_SIDE=client \
  QCC_PANEL_PORT=9998 \
  QCC_PANEL_USER=admin \
  QCC_REPO=pechenyeru/quiccochet \
  bash -c "$(curl -fsSL ${PANEL_URL}/install.sh)" install

# Patch client.json with the foreign IP and pubkey
sed -i "s|REPLACE_WITH_FOREIGN_IP|$FOREIGN_IP|g; s|REPLACE_WITH_FOREIGN_PUBLIC_KEY|$FOREIGN_PUB|g" /etc/quiccochet/client.json

# Set agent token & foreign panel for the iran panel
python3 -c "
import json
p='/etc/quiccochet/panel.json'
with open(p) as f: c = json.load(f)
c['agent_token'] = '$AGENT_TOKEN'
c['foreign_panel'] = '$PANEL_URL'
with open(p,'w') as f: json.dump(c, f, indent=2)
" 2>/dev/null || true

systemctl restart quiccochet quiccochet-panel
IRAN

  ok "qcc-ctl + iran template نصب شدن"
}

print_creds() {
  local creds="$CONFIG_DIR/.creds"
  [[ -f "$creds" ]] || return
  source "$creds"
  echo
  echo -e "${G}╔══════════════════════════════════════════════════╗${N}"
  echo -e "${G}║${N}  ${B}QUICochet نصب شد${N} ${C}(${SIDE})${N}                       ${G}║${N}"
  echo -e "${G}╚══════════════════════════════════════════════════╝${N}"
  echo
  echo -e "  ${C}پنل وب:${N}        ${B}${PANEL_URL}${N}"
  echo -e "  ${C}نام کاربری:${N}    ${ADMIN_USER}"
  echo -e "  ${C}رمز عبور:${N}      ${B}${PANEL_PASS}${N}"
  echo -e "  ${C}Public Key:${N}    ${PUBLIC_KEY:0:48}..."
  echo -e "  ${C}Agent Token:${N}   ${AGENT_TOKEN:0:24}..."
  echo
  if [[ "$SIDE" == "server" ]]; then
    echo -e "  ${Y}مرحله بعدی — نصب سرور ایران:${N}"
    echo -e "    ${B}sudo qcc-ctl iran-installer${N}"
    echo
  fi
  echo -e "  ${C}دستورها:${N}"
  echo -e "    qcc-ctl status    — وضعیت کامل (سرویس + admin stats)"
  echo -e "    qcc-ctl logs      — لاگ زنده"
  echo -e "    qcc-ctl restart   — ری‌استارت"
  echo
}

cmd_install() {
  log "نصب QUICochet ($SIDE)"
  install_deps
  apply_sysctl
  download_release
  generate_config
  install_services
  configure_firewall
  install_ctl
  print_creds
}

cmd_update() {
  log "آپدیت..."
  systemctl stop ${NAME} ${NAME}-panel 2>/dev/null || true
  download_release
  systemctl start ${NAME} ${NAME}-panel 2>/dev/null || true
  ok "آپدیت تمام: $($TUN_BIN --version 2>/dev/null || echo unknown)"
}

cmd_remove() {
  log "حذف..."
  read -rp "آیا مطمئنی؟ [y/N] " ans
  [[ "${ans,,}" == "y" ]] || { warn "لغو"; exit 0; }
  systemctl stop    ${NAME} ${NAME}-panel 2>/dev/null || true
  systemctl disable ${NAME} ${NAME}-panel 2>/dev/null || true
  rm -f "$SVC_TUNNEL" "$SVC_PANEL"
  systemctl daemon-reload
  rm -f "$TUN_BIN" "$PANEL_BIN" "$CTL"
  rm -rf /usr/local/share/quiccochet
  rm -f /etc/sysctl.d/99-${NAME}.conf
  read -rp "حذف کانفیگ‌ها هم؟ [y/N] " a2
  if [[ "${a2,,}" == "y" ]]; then
    rm -rf "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" "$RUN_DIR"
    ok "همه چیز حذف شد"
  else
    warn "کانفیگ‌ها در $CONFIG_DIR نگه‌داشته شد"
  fi
}

cmd_status() {
  echo -e "${B}=== QUICochet Status ===${N}"
  for svc in ${NAME} ${NAME}-panel; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
      ts=$(systemctl show -p ActiveEnterTimestamp --value "$svc" 2>/dev/null)
      echo -e "  $svc: ${G}● active${N}  $ts"
    else
      echo -e "  $svc: ${R}○ inactive${N}"
    fi
  done
  if [[ -x "$TUN_BIN" ]]; then
    echo -e "  tunnel binary: $TUN_BIN ($($TUN_BIN --version 2>/dev/null | head -1 || echo '?'))"
  fi
  if [[ -x "$PANEL_BIN" ]]; then
    echo -e "  panel binary:  $PANEL_BIN ($($PANEL_BIN version 2>/dev/null || echo '?'))"
  fi
  if [[ -f "$CONFIG_DIR/.creds" ]]; then
    source "$CONFIG_DIR/.creds"
    echo -e "  side:  ${SIDE:-?}"
    echo -e "  panel: ${PANEL_URL:-?}"
  fi
  echo
  echo -e "${B}=== Live tunnel stats ===${N}"
  "$TUN_BIN" admin stats 2>/dev/null || echo "  (admin socket not reachable — daemon not running?)"
}

cmd_iran_installer() { /usr/local/bin/qcc-ctl iran-installer; }

# --- Main ---
case "${1:-}" in
  install)            cmd_install ;;
  update|upgrade)     cmd_update ;;
  remove|uninstall)   cmd_remove ;;
  status)             cmd_status ;;
  iran-installer)     cmd_iran_installer ;;
  *)
    cat <<HELP
QUICochet Unified Installer

استفاده:
  curl -fsSL <URL>/install.sh | sudo bash -s -- install
  curl -fsSL <URL>/install.sh | sudo bash -s -- update
  curl -fsSL <URL>/install.sh | sudo bash -s -- remove
  curl -fsSL <URL>/install.sh | sudo bash -s -- status
  curl -fsSL <URL>/install.sh | sudo bash -s -- iran-installer

متغیرهای محیطی (اختیاری):
  QCC_VERSION       نسخه (پیش‌فرض: latest)
  QCC_SIDE          server | client (پیش‌فرض: server)
  QCC_PANEL_PORT    پورت پنل (پیش‌فرض: 9999)
  QCC_TUNNEL_PORT   پورت تونل (پیش‌فرض: 8443)
  QCC_PANEL_USER    یوزر admin (پیش‌فرض: admin)
  QCC_REPO          مسیر گیتهاب (پیش‌فرض: pechenyeru/quiccochet)
HELP
    exit 1
    ;;
esac

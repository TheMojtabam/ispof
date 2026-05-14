# Ispof

> Admin panel for [QUICochet](https://github.com/TheMojtabam/ispof) — manage tunnel configurations, lifecycle, keys, and logs from a single self-hosted web UI.

[![build](https://github.com/TheMojtabam/ispof/actions/workflows/build.yml/badge.svg)](https://github.com/TheMojtabam/ispof/actions/workflows/build.yml)
[![Go version](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## What this is

Ispof is the **administrative UI + control plane** for QUICochet tunnels. It is a single Go binary that:

- stores tunnel configurations as JSON files in `/etc/ispof/tunnels/`
- drives tunnel lifecycle through systemd template units (`quiccochet@<name>.service`)
- generates X25519 key pairs (real — `crypto/ecdh`, not mocked)
- streams live unit state to the browser via Server-Sent Events
- exposes a REST API at `/api/*` for everything the UI does

The actual network plumbing — packet spoofing, QUIC framing, ICMP/UDP/RAW transports — is handled by the underlying **quiccochet** binary, which Ispof launches as a child process via systemd. Ispof is the conductor, not the orchestra.

## What this is NOT

- **Not a tunnel daemon.** Without the `quiccochet` binary installed, the panel will start, list/create configs, generate keys, and look beautiful, but `systemctl start quiccochet@<name>` will fail with "exec format error" or similar because there is no daemon to execute. See [Requirements](#requirements).
- **Not a packet processor.** Traffic never flows through Ispof. The panel runs as the `ispof` user with no network privileges of its own.

## Status

The control plane is **production-ready**:

| Capability                                      | Status                          |
|-------------------------------------------------|---------------------------------|
| Tunnel config CRUD (create/read/update/delete)  | ✅ real, persistent in JSON     |
| Process lifecycle (start/stop/restart/enable)   | ✅ real, via systemctl          |
| X25519 key generation                           | ✅ real, `crypto/ecdh` stdlib   |
| Live state streaming (SSE)                      | ✅ real, 2-second tick          |
| Log viewing (single fetch)                      | ✅ real, reads journalctl       |
| Log streaming (live tail)                       | ✅ real, journalctl -f over SSE |
| Prometheus metric scraping                      | ✅ real, per-tunnel `/metrics`  |
| Per-tunnel rate calculation (client-side)       | ✅ real, counter delta diffing  |
| Event log with state-change detection           | ✅ real, in-memory ring buffer  |
| Basic auth                                      | ✅ constant-time compare        |
| systemd hardening (NoNewPrivileges, etc.)       | ✅ applied                      |
| Multi-user / RBAC                               | ⏳ not yet — single admin only  |
| TLS termination                                 | ⏳ delegate to reverse proxy    |

Everything in the UI now reads from real data sources. The dashboard's throughput chart plots counter deltas from `quiccochet_bytes_total`, the packet-loss chart plots `quiccochet_packets_dropped_total` rate, the QUIC pool widget reads `quiccochet_quic_pool_active` and `quiccochet_active_sessions`, and the events list reads from the in-memory event log fed by lifecycle and state-change observations. **No placeholder values, no hard-coded fixtures, no fake "healthy" badges.**

---

## Requirements

- Linux with systemd ≥ 240
- `quiccochet` binary installed at `/usr/local/bin/quiccochet` (for tunnels to actually carry traffic)
- root for installation; the panel itself runs as the unprivileged `ispof` user

## Install

### Quick (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/TheMojtabam/ispof/main/scripts/install.sh | sudo bash -s install
```

The installer:

1. detects your architecture (amd64 / arm64 / armv7)
2. downloads the matching Ispof release tarball (uses the rolling `latest` pre-release built by CI on every push to `main`, so you always get the freshest build without waiting for a tagged release)
3. verifies the sha256 sidecar
4. creates the `ispof` system user
5. installs the binary to `/usr/local/bin/ispof`
6. writes systemd units: `ispof.service` (panel) and `quiccochet@.service` (tunnel template)
7. **auto-discovers the quiccochet tunnel binary** — see [Tunnel auto-discovery](#tunnel-auto-discovery) below
8. generates a random basic-auth password (printed once at the end — save it!)
9. enables and starts the panel

When it finishes, open `http://127.0.0.1:2095/` and log in with `admin` + the generated password.

### Tunnel auto-discovery

Ispof is the control plane; the actual `quiccochet` tunnel binary is a separate program that has to exist on the server for tunnels to start. The installer doesn't assume any specific location — it walks through this pipeline:

| Step | What it does | Cost |
|------|--------------|------|
| 0 | Honors `QUICCOCHET_PATH=...` if you set it | instant |
| 0.5 | Honors `QUICCOCHET_OFFLINE=...` (path to binary or source tree) | instant |
| 1 | Checks `/usr/local/bin/quiccochet` | instant |
| 2 | Checks PATH | instant |
| 3 | Checks ~12 common directories (`/usr/local/bin`, `/opt/quiccochet`, `/home/*/bin`, `/home/*/go/bin`, ...) | instant |
| 4 | **Scans the whole root filesystem** for files named `quiccochet` (60s budget, single-fs, skips `/proc`, `/sys`, `/var/lib/docker`) | up to 60s |
| 5 | Searches the filesystem for a `go.mod` referencing `quiccochet` (the source tree), and if Go is installed, builds it via `go build` | up to 60s + build time |
| 6 | If online, downloads the binary from `github.com/pechenyeru/quiccochet/releases/latest` — tries 4 filename conventions, falls back to git-clone + go-build | network-dependent |
| 7 | If nothing worked, prints clear instructions for manual install. The panel still installs and starts normally — you just can't start tunnels until quiccochet is present. | instant |

If found at a non-canonical path, the binary is copied to `/usr/local/bin/quiccochet` so the systemd template unit can locate it consistently.

### GitHub mirrors for restricted networks

If `github.com` is blocked on your network (common in IR / CN), the installer automatically falls back through this mirror list:

```
https://github.com
https://ghproxy.com/https://github.com
https://gh-proxy.com/https://github.com
https://gh.api.99988866.xyz/https://github.com
https://mirror.ghproxy.com/https://github.com
```

Override with `GITHUB_MIRRORS=base1:base2:base3`.

### Standalone tunnel install / re-discovery

If the panel is already installed but `quiccochet` was missing at install time (you've since added it elsewhere on the server, or the GitHub download failed earlier), run:

```bash
sudo bash install.sh install-quiccochet
```

And to debug what discovery finds without changing anything:

```bash
sudo bash install.sh find-quiccochet
```

### Environment overrides

| Env var | What it does |
|---------|--------------|
| `ISPOF_VERSION` | Pin a release tag (default: `latest`) |
| `ISPOF_LISTEN` | Bind address (default: `127.0.0.1:2095`) |
| `ISPOF_AUTH` | Basic auth `user:pass` — auto-generated if blank |
| `ISPOF_OFFLINE` | Path to local Ispof tarball (air-gapped install) |
| `ISPOF_NO_AUTOSTART` | Don't enable/start the service |
| `ISPOF_SKIP_QUICCOCHET` | Skip the tunnel-binary auto-discovery step |
| `QUICCOCHET_REPO` | Override the GitHub repo (default: `pechenyeru/quiccochet`) |
| `QUICCOCHET_PATH` | Skip discovery — use this binary path directly |
| `QUICCOCHET_OFFLINE` | Path to local quiccochet binary or source tree (air-gapped) |
| `GITHUB_MIRRORS` | Colon-separated GitHub mirror bases |

### Air-gapped install

For machines with no internet at all:

```bash
# On a connected machine:
wget https://github.com/TheMojtabam/ispof/releases/latest/download/ispof-latest-linux-amd64.tar.gz
wget https://github.com/pechenyeru/quiccochet/releases/latest/download/quiccochet-linux-amd64

# Copy both to the target server, then:
ISPOF_OFFLINE=/path/to/ispof-latest-linux-amd64.tar.gz \
QUICCOCHET_OFFLINE=/path/to/quiccochet-linux-amd64 \
  sudo bash install.sh install
```

Or hand it a source tree to build:

```bash
QUICCOCHET_OFFLINE=/path/to/quiccochet-source-tree \
  sudo bash install.sh install
```

### Update

```bash
sudo bash install.sh update
```

### Uninstall

```bash
sudo bash install.sh uninstall
```

Asks before removing `/etc/ispof/` and `/usr/local/bin/quiccochet`.

### Status

```bash
sudo bash install.sh status
```

Shows the panel state, the quiccochet binary status, and active tunnel units.

---

## Running from source

```bash
git clone https://github.com/TheMojtabam/ispof.git
cd ispof
make build
./ispof --listen 127.0.0.1:2095 --log-level debug
```

The HTML is embedded at compile time — no asset directory to deploy.

### Build flags

| Flag                | Default                      | Notes                                              |
|---------------------|------------------------------|----------------------------------------------------|
| `--listen`          | `127.0.0.1:2095`             | Bind address. Use `0.0.0.0:` with `--auth` only.   |
| `--tunnels-dir`     | `/etc/ispof/tunnels`         | JSON config files live here.                       |
| `--keys-dir`        | `/etc/ispof/keys`            | Private keys are written here (mode 0600).         |
| `--log-level`       | `info`                       | `debug` / `info` / `warn` / `error`                |
| `--auth`            | `""` (no auth)               | `user:password` — required for non-loopback bind.  |
| `--version`         |                              | Print version and exit.                            |

---

## REST API

All endpoints accept and return JSON. Authentication is HTTP Basic when `--auth` is configured.

### Tunnels

| Method | Path                                       | Description                                     |
|--------|--------------------------------------------|-------------------------------------------------|
| GET    | `/api/tunnels`                             | List every tunnel + state + scrape snapshot     |
| POST   | `/api/tunnels`                             | Create a new tunnel from a JSON body            |
| GET    | `/api/tunnels/{name}`                      | Single tunnel + state + scrape snapshot         |
| PUT    | `/api/tunnels/{name}`                      | Replace a tunnel's config                       |
| DELETE | `/api/tunnels/{name}`                      | Stop + disable + delete (key file too)          |
| POST   | `/api/tunnels/{name}/start`                | `systemctl start quiccochet@{name}`             |
| POST   | `/api/tunnels/{name}/stop`                 | `systemctl stop quiccochet@{name}`              |
| POST   | `/api/tunnels/{name}/restart`              | `systemctl restart quiccochet@{name}`           |
| POST   | `/api/tunnels/{name}/enable`               | Persist across boots                            |
| POST   | `/api/tunnels/{name}/disable`              | Un-persist (does not stop running unit)         |
| GET    | `/api/tunnels/{name}/logs?lines=200`       | Last N lines from journalctl                    |
| GET    | `/api/tunnels/{name}/stream/logs`          | SSE stream of journalctl -f                     |
| GET    | `/api/tunnels/{name}/state`                | Live systemd state                              |
| GET    | `/api/tunnels/{name}/metrics`              | Latest Prometheus scrape snapshot               |
| GET    | `/api/tunnels/{name}/history`              | Full scrape ring buffer (60 samples)            |

### System

| Method | Path                  | Description                                            |
|--------|-----------------------|--------------------------------------------------------|
| GET    | `/api/version`        | Version, commit, build date, runtime                   |
| GET    | `/api/system`         | Hostname, goroutines, paths                            |
| POST   | `/api/keygen`         | Generate X25519 pair (real)                            |
| GET    | `/api/events?n=50`    | Recent lifecycle + state-change events                 |
| GET    | `/api/stream/state`   | SSE: pushes tunnels + state + scrape + events every 2s |
| GET    | `/healthz`            | 200 OK liveness probe (unauthenticated)                |

### Example: create a client tunnel

```bash
curl -u admin:secret -X POST http://127.0.0.1:2095/api/tunnels \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "vpn-de",
    "mode": "client",
    "transport": {"type": "udp"},
    "server":    {"address": "203.0.113.10", "port": 8080},
    "spoof":     {"source_ips": ["10.20.30.40"], "peer_spoof_ips": ["10.20.30.50"]},
    "crypto":    {"private_key_file": "BASE64_X25519_KEY", "peer_public_key": "BASE64_PEER"},
    "performance": {"mtu": 1400},
    "quic":       {"pool_size": 8, "congestion_control": "auto"},
    "obfuscation":{"mode": "standard"},
    "security":   {"block_private_targets": true},
    "logging":    {"level": "info"},
    "admin":      {"enabled": true},
    "metrics":    {"enabled": true, "listen": "127.0.0.1:9200"}
  }'
```

The panel detects that `private_key_file` is a raw base64 X25519 key and writes it to `/etc/ispof/keys/vpn-de.key` automatically.

---

## On-disk layout

```
/usr/local/bin/ispof                          # the panel binary
/etc/ispof/
├── tunnels/
│   ├── vpn-de.json                           # one config per tunnel
│   └── server-frankfurt.json
└── keys/
    ├── vpn-de.key                            # 0600, owned by ispof
    └── server-frankfurt.key
/etc/default/ispof                            # environment file
/etc/systemd/system/ispof.service             # panel unit
/etc/systemd/system/quiccochet@.service       # tunnel template
```

The JSON files in `/etc/ispof/tunnels/` are **the same format `quiccochet -c` reads**. The panel does not translate or wrap them — what you see in the editor is what the daemon gets.

## Security

- The panel runs as a non-root user (`ispof`). It does NOT have raw socket or CAP_NET_ADMIN privileges. Tunnel daemons run separately under their own unit instances.
- All systemctl/journalctl calls are made via the user's shell. To restrict what the panel can manage, drop a `polkit` rule limiting the `ispof` user to the `quiccochet@*` unit family.
- Basic auth uses constant-time comparison. Behind a reverse proxy you can terminate TLS and route only `Basic`/`Bearer` requests through.
- The `/healthz` endpoint is intentionally unauthenticated so probes work before credentials are configured.

## Development

```bash
make build      # produce ./ispof
make run        # build + run with debug logging
make test       # unit tests
make release    # cross-compile linux/{amd64,arm64,armv7} into dist/
make tag VERSION=v0.2.0   # tag + push (CI publishes stable release)
```

The frontend lives in `internal/webui/assets/index.html`. There is no build step — edit the file, rebuild, refresh. The Go `embed` directive bakes the latest copy into the binary at build time.

## Releases & CI

This repo's GitHub Actions workflow publishes binaries automatically:

| Trigger | What happens |
|---------|--------------|
| Push to `main` | Cross-compiles all three Linux architectures, replaces the `latest` rolling pre-release at `releases/latest`. The install script defaults to this. |
| Push of a `v*` tag | Cross-compiles, publishes a stable GitHub release with auto-generated release notes. |
| Manual `workflow_dispatch` with a `release_tag` input | Same as a tag push, but you don't need to push a tag. |
| Pull request | Builds only, no release. |

So three ways to cut a release:

```bash
# 1. Easiest: just push to main — "latest" pre-release rebuilds.
git push origin main

# 2. Tag-driven stable release.
make tag VERSION=v0.2.0

# 3. Manual via Actions UI: workflow_dispatch with the desired tag.
```

Every release artifact has a `.sha256` sidecar; the install script verifies it automatically.

## Project layout

```
cmd/ispof/main.go               — entry point, flag parsing, wiring
internal/api/handlers.go        — REST + SSE handlers
internal/store/store.go         — tunnel persistence + validation
internal/procmgr/manager.go     — systemctl/journalctl shell-out
internal/cryptoutil/keygen.go   — X25519 key generation
internal/webui/server.go        — middleware + asset serving
internal/webui/assets/index.html — the entire SPA
systemd/                        — unit files
scripts/install.sh              — installer / updater / uninstaller
.github/workflows/build.yml     — CI: build + release on tag
```

## Roadmap

- [x] Real config CRUD with atomic writes
- [x] Real systemd integration (start/stop/restart/enable/disable)
- [x] Real X25519 keygen
- [x] Real log streaming
- [x] SSE for live state
- [ ] Prometheus scrape per tunnel (replace memory-RSS chart)
- [ ] WebSocket log tailing (currently last-N fetch)
- [ ] Multi-user with role-based access
- [ ] TLS termination built-in (currently delegate to reverse proxy)

## License

MIT

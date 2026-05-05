# QUICochet Web Panel

پنل وب کامل برای مدیریت تونل QUICochet — مکمل CLI اصلی، با UI تمیز و RTL و فونت Arad.

این یک ضمیمه به پروژه اصلی QUICochet است. پنل به admin Unix socket تونل (همون که `quiccochet admin stats` ازش استفاده می‌کنه) متصل می‌شه و یک HTTP API + UI روی پورت ۹۹۹۹ ارائه می‌ده.

```
                 ┌──────────────────────────────────┐
                 │        Browser / Mobile          │
                 │  http://server-ip:9999  (RTL)    │
                 └────────────────┬─────────────────┘
                                  │ HTTPS / HTTP
                 ┌────────────────▼─────────────────┐
                 │       qcc-panel binary           │
                 │  - chi HTTP router               │
                 │  - JWT + bcrypt + 2FA TOTP       │
                 │  - file-based JSON store         │
                 │  - WebSocket live broadcast      │
                 └────────────────┬─────────────────┘
                                  │ /run/quiccochet/admin.sock
                 ┌────────────────▼─────────────────┐
                 │       quiccochet daemon          │
                 │  - QUIC tunnel (existing)        │
                 │  - IP spoofing                   │
                 │  - admin command socket          │
                 └──────────────────────────────────┘
```

---

## دو باینری، یک پروژه

از این تگ به بعد، پروژه دو باینری build می‌کنه:

| باینری | کار |
|--------|-----|
| `quiccochet` | همون daemon قبلی — تونل QUIC، spoofing، admin socket |
| `qcc-panel` | جدید — وب پنل، HTTP API، مدیریت user/inbound/routing |

هر دو در یک release tarball قرار می‌گیرن.

---

## نصب یک خطی

روی سرور **خارج** (exit node):

```bash
curl -fsSL https://github.com/pechenyeru/quiccochet/releases/latest/download/install.sh \
  | sudo QCC_SIDE=server bash -s -- install
```

روی سرور **ایران** (ساده‌ترین مسیر — اول خارج رو نصب کن، بعد):

```bash
# روی خارج بعد از نصب:
sudo qcc-ctl iran-installer
# اسکریپت اختصاصی روی $PUBLIC_IP:9999/installer/iran-XXXXXX.sh قرار می‌گیره

# روی ایران:
curl -fsSL http://<FOREIGN_IP>:9999/installer/iran-XXXXXX.sh | sudo bash
```

اسکریپت ایران یک‌بار-مصرف هست و به صورت hardcode شامل:
- IP خارج
- کلید عمومی X25519 خارج
- agent token

نصب می‌کنه:
- باینری tunnel (به‌عنوان client)
- پنل ایران روی پورت ۹۹۹۸
- پورت ۴۴۳ TCP باز
- systemd unit ها

---

## ویژگی‌های پنل

### تونل (با ادغام کامل با daemon موجود)

پنل به admin Unix socket داره وصل می‌شه و:

- وضعیت زنده تونل از همون `Snapshot` interface (`internal/admin/admin.go`):
  - `pool_alive`, `pool_total`, `udp_assocs`, `active_sessions`
  - `bytes_sent` / `bytes_received` (cumulative)
  - `packets_sent` / `packets_lost` → نرخ خطا
  - `uptime_sec` تونل
- نرخ Up/Down (Mbps) از delta شمارنده‌ها (panel در tick دوم محاسبه می‌کنه)
- bench زنده — صدا زدن `bench latency|throughput` از طریق socket (در آینده)

تنظیمات تونل (write-through به `/etc/quiccochet/server.json`):
- transport: UDP / ICMP (echo|reply) / RAW / SYN+UDP
- spoof source/peer IP
- crypto private/public key
- obfuscation mode + padding
- QUIC pool size, congestion (BBR/CUBIC)
- security: replay protection, block private targets

پس از تغییر کانفیگ، پنل سیگنال SIGHUP به daemon می‌فرسته (یا restart می‌کنه — انتخاب کاربر).

### Xray inbounds و کاربران
- پروتکل: VLESS / VMess / Trojan / SS-2022
- ترنسپورت: TCP / WS / gRPC / **REALITY**
- CRUD کاربر: quota، expiry، گروه، UUID
- subscription URL با `Subscription-Userinfo` header
- QR code (PNG)
- export CSV
- regenerate خودکار `xray.json`

### routing
- domain / IP / protocol / port rules
- direct / block / proxy outbound
- GeoIP و GeoSite (skeleton)

### امنیت پنل
- bcrypt (cost 12) password hash
- HMAC-SHA256 session token (JWT-shaped)
- 2FA TOTP (RFC 6238) با QR
- IP whitelist (CIDR)
- per-IP login rate limit (5/5min)
- API tokens برای automation

### مدیریت سیستم
- sysctl tuning apply (BBR، fq، rmem/wmem)
- firewall: ufw / firewalld / iptables
- service control (whitelisted: quiccochet, quiccochet-panel, xray)
- log tail از journalctl
- system info: kernel، CPU، RAM، disk، interfaces

### اطلاع‌رسانی
- Telegram bot
- Discord webhook
- ارسال خودکار event ها

### بک‌آپ
- بک‌آپ از `/etc/quiccochet` به tar.gz
- لیست با size/date
- restore با تایید

### Iran ↔ Foreign link
- agent token authentication
- iran heartbeat → foreign
- iran metrics push
- ساخت اسکریپت نصب اختصاصی برای ایران

### Live updates
- WebSocket broadcast وضعیت تونل و iran link

---

## ساختار پنل در ریپو

```
qcc-panel/
├── cmd/qcc-panel/        # entry point (panel binary)
│   ├── main.go           # CLI: panel | genkey | pubkey | hash-password
│   └── keys.go           # X25519 helpers
├── internal/panel/
│   ├── api/              # HTTP handlers (chi)
│   │   ├── api.go              # routes wiring
│   │   ├── auth_mw.go          # JWT middleware + login
│   │   ├── tunnel.go           # tunnel CRUD + restart via systemd
│   │   ├── users.go
│   │   ├── inbounds.go
│   │   ├── sub.go              # subscription URL + QR
│   │   ├── routing.go          # rules + forwards + dashboard
│   │   ├── system.go           # sysinfo / sysctl / firewall / services
│   │   ├── iran.go             # iran-link, agent endpoints, backups
│   │   ├── ws.go               # websocket upgrade
│   │   ├── exec.go
│   │   └── util.go
│   ├── auth/             # bcrypt + JWT-HMAC + TOTP
│   ├── config/           # panel config (separate from tunnel config)
│   ├── store/            # JSON file-based KV
│   ├── tunnel/           # admin socket client + tunnel config CRUD
│   ├── xray/             # xray config emitter
│   ├── sys/              # /proc, sysctl, firewall
│   ├── ws/               # websocket hub
│   └── notify/           # telegram + discord
└── web/                  # //go:embed UI files
    ├── server-panel.html
    ├── client-panel.html
    ├── panel-base.css
    └── arad-font.css
```

### Wiring با تونل اصلی

پنل از سه مسیر با daemon ارتباط داره:

1. **`/run/quiccochet/admin.sock`** — برای فکچر کردن stats زنده.
   فرمت: یک خط دستور، یک خط جواب JSON.
   پنل پشت `internal/panel/tunnel/Manager.queryAdmin()` این رو wrap می‌کنه.

2. **`/etc/quiccochet/{server,client}.json`** — کانفیگ تونل.
   پنل می‌خونه و می‌نویسه. daemon هنگام restart می‌خونه.

3. **systemctl** — وقتی پنل کانفیگ تونل رو تغییر بده، می‌تونه از پنل دکمه «اعمال» بزنی که:
   - `systemctl restart quiccochet` (سخت‌ترین مسیر، تونل قطع می‌شه)
   - یا `systemctl reload quiccochet` اگه daemon از SIGHUP پشتیبانی کنه

---

## ساخت دستی

پروژه با Go 1.25+ کامپایل می‌شه (به‌خاطر مشخصات quic-go fork):

```bash
git clone https://github.com/pechenyeru/quiccochet
cd quiccochet
make           # build هر دو
# یا جداگانه:
go build -ldflags '-s -w' -o quiccochet  ./cmd/quiccochet
go build -ldflags '-s -w' -o qcc-panel   ./cmd/qcc-panel
```

## کانفیگ نمونه پنل

```json
{
  "side": "server",
  "listen": "0.0.0.0:9999",
  "admin_user": "admin",
  "admin_pass": "$2y$12$...",
  "agent_token": "openssl-rand-hex-24-output",
  "data_dir": "/var/lib/quiccochet",
  "log_dir": "/var/log/quiccochet",
  "tunnel_config": "/etc/quiccochet/server.json",
  "xray_config": "/etc/xray/config.json",
  "rate_limit_logins": true,
  "session_timeout_min": 30,
  "ip_whitelist": ["0.0.0.0/0"],
  "telegram": {
    "enabled": false,
    "bot_token": "",
    "chat_id": ""
  },
  "discord": {
    "enabled": false,
    "webhook_url": ""
  }
}
```

## کانفیگ نمونه تونل (حداقلی)

این کانفیگ از قبل در پروژه بود (`server-config.json.example`)؛ پنل فقط از همین اسکیما استفاده می‌کنه.

```json
{
  "mode": "server",
  "transport": { "type": "udp" },
  "listen_port": 8443,
  "spoof": {
    "source_ip": "10.99.0.1",
    "peer_spoof_ip": "10.99.0.2",
    "client_real_ip": "0.0.0.0"
  },
  "crypto": {
    "private_key": "<base64 X25519>",
    "peer_public_key": "<base64 X25519>"
  },
  "obfuscation": { "enabled": true, "mode": "standard" },
  "security": { "block_private_targets": true },
  "admin": { "socket_path": "/run/quiccochet/admin.sock" },
  "logging": { "level": "info", "file": "/var/log/quiccochet/tunnel.log" }
}
```

---

## وضعیت قابلیت‌ها

| لایه | قابلیت | پیاده‌سازی |
|------|--------|-----------|
| پنل | login/JWT/2FA/rate-limit | ✅ کامل |
| پنل | CRUD کاربر/inbound/routing/forward | ✅ کامل |
| پنل | subscription + QR | ✅ کامل |
| پنل | regenerate xray.json | ✅ کامل |
| پنل | WebSocket live | ✅ کامل |
| پنل | sysctl/firewall/service | ✅ کامل |
| پنل | Telegram + Discord notify | ✅ کامل |
| پنل | iran installer generator | ✅ کامل |
| پنل | backup tar.gz (create+list+delete) | ✅ کامل |
| تونل | همه موارد projet اصلی (UDP/ICMP/RAW/SYN+UDP, spoof, BBR, ...) | ✅ کامل (از QUICochet اصلی) |
| اتصال | پنل ↔ admin socket تونل (live stats) | ✅ کامل |
| اتصال | iran ↔ foreign agent heartbeat | ✅ کامل |
| اتصال | پنل → SIGHUP/restart daemon | ✅ via systemctl |
| stub | restore backup (extract tar) | ⚠️ TODO |
| stub | GeoIP/GeoSite download | ⚠️ TODO |
| stub | iperf3 bench واقعی (پنل-side) | ⚠️ TODO (تونل خودش `admin bench` داره — می‌شه pipe کرد) |
| stub | برنامه‌ریزی iptables برای forwards | ⚠️ persist می‌شه ولی rule نمی‌نویسه |

---

## License

همان MIT — مشترک با پروژه اصلی.

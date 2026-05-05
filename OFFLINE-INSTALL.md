# نصب آفلاین روی سرور ایران (بدون اینترنت)

این راهنما برای زمانی است که سرور ایران اصلاً اینترنت نداره (مثل سرورهای بسته در پشت فایروال یا VPS هایی که دسترسی خروجی محدود دارن).

## فلو کلی

```
┌──────────────────┐  scp file.tar.gz   ┌─────────────────────┐
│ کامپیوتر شخصی    │ ─────────────────► │ سرور ایران          │
│ (با اینترنت)     │                    │ (بدون اینترنت)      │
└────────┬─────────┘                    └─────────────────────┘
         │
         │ wget از GitHub
         ▼
   GitHub Release
   quiccochet-linux-amd64.tar.gz
```

## مرحله ۱: ساخت Release روی GitHub

اولین بار که می‌خوای release بسازی:

```bash
# روی کامپیوتر خودت، از داخل ریپو:
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions خودش (workflow `release.yml`):
- پروژه رو برای ۳ معماری build می‌کنه: amd64, arm64, armv7
- هر کدوم رو در یه `quiccochet-linux-XXX.tar.gz` بسته‌بندی می‌کنه
- یه release با همه فایل‌ها می‌سازه

می‌تونی پیشرفت رو در `https://github.com/YOUR_USER/YOUR_REPO/actions` ببینی. حدود ۲ تا ۴ دقیقه طول می‌کشه.

## مرحله ۲: دانلود tarball روی کامپیوتر شخصی

```bash
# معمولاً سرور VPS x86_64 = amd64
wget https://github.com/YOUR_USER/YOUR_REPO/releases/latest/download/quiccochet-linux-amd64.tar.gz
```

برای سرورهای ARM:
```bash
# Oracle Cloud ARM، Raspberry Pi 4/5
wget https://github.com/YOUR_USER/YOUR_REPO/releases/latest/download/quiccochet-linux-arm64.tar.gz
```

اگه مطمئن نیستی، این رو روی سرور بزن: `uname -m`. اگه `x86_64` بود → `amd64`. اگه `aarch64` بود → `arm64`.

## مرحله ۳: کپی به سرور ایران

```bash
scp quiccochet-linux-amd64.tar.gz root@<IRAN_IP>:/tmp/
```

## مرحله ۴: نصب آفلاین روی سرور ایران

```bash
ssh root@<IRAN_IP>

# روی سرور ایران:
cd /tmp
tar -xzf quiccochet-linux-amd64.tar.gz
cd quiccochet-linux-amd64/

# نصب — اسکریپت خودش متوجه می‌شه باینری‌ها کنارش هستن و دانلود رو رد می‌کنه
sudo QCC_SIDE=client ./install.sh install
```

اسکریپت چیکار می‌کنه:
1. ✓ چک می‌کنه دو ابزار ضروری (`tar` و `openssl`) موجودن — این‌ها روی هر Linux معمولی از قبل هستن
2. ✓ متوجه می‌شه باینری‌ها کنارش هستن، **هیچ دانلودی نمی‌کنه** — نه Go، نه git، نه apt-get update
3. ✓ باینری‌های `quiccochet` و `qcc-panel` رو کپی می‌کنه به `/usr/local/bin/`
4. ✓ کانفیگ `client.json` و `panel.json` می‌سازه
5. ✓ سرویس‌های systemd ثبت می‌کنه
6. ✓ پنل ایران رو روی پورت ۹۹۹۸ بالا میاره
7. ✓ پسورد admin تصادفی می‌سازه و چاپ می‌کنه

### پیش‌نیازهای مطلق روی سرور ایران

این‌ها روی **هر** نصب Linux معمولی (حتی minimal Ubuntu Server) از قبل هستن:

| ابزار | کاربرد | بدون این چی می‌شه |
|------|--------|------------------|
| `bash` | اجرای اسکریپت | اسکریپت اصلاً اجرا نمی‌شه |
| `tar` | extract آرشیو | باید روی کامپیوتر دیگه extract کنی و فقط دایرکتوری رو scp کنی |
| `openssl` | تولید کلید X25519 و توکن | اسکریپت stop می‌ده |
| `systemctl` | راه‌اندازی سرویس | systemd lazy، روی هر VPS مدرن هست |

**نیازی به این‌ها نیست** (در حالت آفلاین):
- ❌ Go compiler — باینری از قبل کامپایل شده
- ❌ git — هیچ clone نمی‌کنه
- ❌ jq — فقط برای parse کردن GitHub API استفاده می‌شه (که ما skip می‌کنیم)
- ❌ curl/wget — برای دانلود استفاده می‌شه (که ما skip می‌کنیم)
- ❌ apt-get update — هیچ پکیجی نصب نمی‌شه

اگه حتی یکی از ابزارهای ضروری گم باشه، اسکریپت یه پیام واضح می‌ده با دستور دقیق `apt install` که می‌تونی روی کامپیوتر شخصیت دانلود کنی و بفرستی به سرور:

```
[ ✗ ] این ابزارهای ضروری گم هستن: openssl
        روی Debian/Ubuntu:  apt install -y openssl
        روی RHEL/CentOS:    yum install -y openssl
```

## مرحله ۵: تنظیم اتصال در پنل ایران

روی مرورگر کامپیوتر خودت (که می‌تونه به سرور ایران دسترسی داشته باشه):

```
http://<IRAN_IP>:9998
```

با `admin` و پسوردی که اسکریپت چاپ کرد لاگین کن.

برو به تب **اتصال تونل → جفت‌سازی** و این‌ها رو پر کن:

| فیلد | مقدار |
|------|-------|
| آدرس IP سرور خارج | IP سرور خارج (مثل `5.75.182.40`) |
| پورت تونل | `8443` |
| نوع ترنسپورت | UDP |
| کلید عمومی X25519 خارج | از `cat /etc/quiccochet/.creds` سرور خارج، مقدار `PUBLIC_KEY` |
| URL پنل خارج | `http://<FOREIGN_IP>:9999` |
| Agent Token | از `.creds` سرور خارج، مقدار `AGENT_TOKEN` |
| Source IP این دستگاه | `10.99.0.2` (پیش‌فرض، می‌تونی عوض کنی) |
| Peer Spoof IP | `10.99.0.1` (باید با Source IP خارج برابر باشه) |

دکمه **تایید و اعمال** رو بزن. اسکریپت:
1. کانفیگ تونل رو write می‌کنه
2. سرویس tunnel رو restart می‌کنه
3. heartbeat رو شروع می‌کنه (هر ۱۵ ثانیه به پنل خارج می‌فرسته)

## مرحله ۶: تایید روی پنل خارج

برو به پنل خارج (`http://<FOREIGN_IP>:9999`)، تب **سرور ایران**. باید ظرف ۱۵–۲۰ ثانیه ببینی:

```
● متصل · iran ip: 192.0.2.x · sessions=N · last seen: ...
```

اگه `○ منتظر heartbeat` نشون داد، یعنی هنوز iran panel نتونسته به foreign panel دسترسی پیدا کنه. چک کن:
- پورت ۹۹۹۹ خارج باز باشه (firewall)
- Agent Token دو طرف یکی باشه
- URL پنل خارج درست باشه (با `http://` یا `https://`)

## دیباگ

```bash
# روی سرور ایران
qcc-ctl status                      # وضعیت سرویس‌ها
qcc-ctl logs                        # لاگ زنده هر دو
journalctl -u quiccochet-panel -f   # فقط لاگ پنل
journalctl -u quiccochet -f         # فقط لاگ تونل

# کانفیگ‌ها
cat /etc/quiccochet/client.json
cat /etc/quiccochet/panel.json
cat /etc/quiccochet/.creds
```

## آپدیت آفلاین

دفعه بعد که می‌خوای آپدیت کنی:
1. tarball نسخه جدید رو دانلود کن
2. `scp` کن به ایران
3. روی ایران: `tar -xzf quiccochet-linux-amd64.tar.gz && cd ... && sudo ./install.sh update`

اسکریپت `update` همون مسیر offline رو طی می‌کنه — فقط باینری‌ها رو جایگزین و سرویس‌ها رو restart می‌کنه.

## چی توی tarball هست؟

```
quiccochet-linux-amd64/
├── quiccochet           ← باینری تونل (~12MB)
├── qcc-panel            ← باینری پنل (~11MB)
├── install.sh           ← نصاب
├── README.md            ← مستند پروژه
├── PANEL.md             ← مستند پنل
├── server-config.json.example
└── client-config.json.example
```

تقریباً ۲۵MB — کاملاً self-contained، نیازی به اینترنت روی سرور ایران نداره.

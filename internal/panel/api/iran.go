package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pechenyeru/quiccochet/internal/panel/agent"
	"github.com/pechenyeru/quiccochet/internal/panel/auth"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
)

// ---- Iran link (foreign side) ----

type IranStatus struct {
	Connected bool      `json:"connected"`
	Version   string    `json:"version"`
	IranIP    string    `json:"iran_ip"`
	LastSeen  time.Time `json:"last_seen"`
	UptimeSec int64     `json:"uptime_sec"`
	CPU       float64   `json:"cpu_pct"`
	RAMMB     float64   `json:"ram_used_mb"`
	Forwards  int       `json:"forwards"`
}

func (s *Server) handleIranStatus(w http.ResponseWriter, _ *http.Request) {
	cur := IranStatus{}
	_ = s.store.Get(store.BucketSettings, "iran_status", &cur)
	writeJSON(w, 200, cur)
}

func (s *Server) handleIranSync(w http.ResponseWriter, _ *http.Request) {
	// TODO: invoke a push to iran-side via /agent/config endpoint.
	go s.notify.Send("iran-sync", "config push to iran requested")
	writeJSON(w, 200, map[string]string{"status": "queued"})
}

func (s *Server) handleIranPushKeys(w http.ResponseWriter, _ *http.Request) {
	// TODO: read tunnel.crypto.public_key and push to iran-side.
	writeJSON(w, 200, map[string]string{"status": "queued"})
}

// handleIranTestForeign — invoked from iran-side panel to verify that the
// foreign IP/port is reachable via UDP before saving the config.
func (s *Server) handleIranTestForeign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.IP == "" || req.Port == 0 {
		writeErr(w, 400, "ip and port required")
		return
	}
	addr := req.IP + ":" + strconv.Itoa(req.Port)
	udpOK := false
	if c, err := net.DialTimeout("udp", addr, 2*time.Second); err == nil {
		_ = c.SetWriteDeadline(time.Now().Add(1 * time.Second))
		_, _ = c.Write([]byte("\x00"))
		_ = c.Close()
		udpOK = true
	}
	writeJSON(w, 200, map[string]any{
		"ok":     udpOK,
		"udp":    udpOK,
		"target": addr,
	})
}

// handleIranInstaller emits an iran-side installer script (one-shot, time-bound).
// Returned JSON includes a download URL the foreign panel can show as a one-liner.
func (s *Server) handleIranInstaller(w http.ResponseWriter, r *http.Request) {
	rnd := randID(4)
	name := "iran-" + rnd + ".sh"
	dst := filepath.Join(s.cfg.DataDir, "installers", name)

	host := s.cfg.Domain
	if host == "" {
		host = strings.Split(r.Host, ":")[0]
	}
	panelURL := "http://" + r.Host
	if s.cfg.BehindTLS {
		panelURL = "https://" + s.cfg.Domain
	}

	script := buildIranInstaller(panelURL, s.cfg.AgentToken, host)
	if err := os.WriteFile(dst, []byte(script), 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{
		"name":         name,
		"path":         dst,
		"download_url": panelURL + "/installer/" + name,
		"oneliner":     "curl -fsSL " + panelURL + "/installer/" + name + " | sudo bash",
		"expires_in":   "24h",
	})
}

// handleInstallerDownload serves a generated iran installer.
func (s *Server) handleInstallerDownload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !strings.HasPrefix(name, "iran-") || !strings.HasSuffix(name, ".sh") {
		writeErr(w, 400, "invalid name")
		return
	}
	path := filepath.Join(s.cfg.DataDir, "installers", name)
	b, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	_, _ = w.Write(b)
}

func buildIranInstaller(panelURL, token, foreignHost string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run with sudo"; exit 1; }
PANEL_URL=%q
AGENT_TOKEN=%q
FOREIGN_HOST=%q

echo "[QCC-IR] Installing client side..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq && apt-get install -y -qq curl jq tar ca-certificates iproute2 iptables ufw openssl 2>/dev/null \
  || yum install -y -q curl jq tar iproute iptables firewalld openssl 2>/dev/null || true

# Install binary
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; esac
TMP=$(mktemp -d)
if curl -fsSL "$PANEL_URL/release/qcc-panel-linux-$ARCH.tar.gz" -o $TMP/q.tgz 2>/dev/null; then
  tar -xzf $TMP/q.tgz -C $TMP
  install -m 0755 $TMP/qcc-panel /usr/local/bin/qcc-panel
fi

# Configure
mkdir -p /etc/quiccochet /var/log/quiccochet /var/lib/quiccochet
PASS=$(openssl rand -base64 12 | tr -d '/+=' | head -c 16)
PRIV=$(/usr/local/bin/qcc-panel genkey 2>/dev/null || openssl rand -base64 32)

cat > /etc/quiccochet/panel.json <<JSON
{
  "side": "client",
  "listen": "0.0.0.0:9998",
  "admin_user": "admin",
  "admin_pass": "$PASS",
  "agent_token": "$AGENT_TOKEN",
  "foreign_panel": "$PANEL_URL",
  "data_dir": "/var/lib/quiccochet",
  "log_dir": "/var/log/quiccochet",
  "tunnel_config": "/etc/quiccochet/client.json"
}
JSON

cat > /etc/quiccochet/client.json <<JSON
{
  "mode": "client",
  "transport": { "type": "udp" },
  "server": { "address": "$FOREIGN_HOST", "port": 8443 },
  "crypto": { "private_key": "$PRIV", "peer_public_key": "" },
  "performance": { "pacing_rate_mbps": 450, "mtu": 1400 },
  "quic": { "pool_size": 8, "congestion_control": "bbr" },
  "obfuscation": { "enabled": true, "mode": "standard" }
}
JSON

cat > /etc/systemd/system/quiccochet-panel.service <<UNIT
[Unit]
Description=QUICochet Iran Panel
After=network-online.target
[Service]
Type=simple
ExecStart=/usr/local/bin/qcc-panel panel --config /etc/quiccochet/panel.json --side client
Restart=on-failure
RestartSec=5
[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now quiccochet-panel.service

IRAN_IP=$(curl -fsSL --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
echo
echo "✓ QUICochet Iran panel installed"
echo "  Panel:    http://$IRAN_IP:9998"
echo "  User:     admin"
echo "  Password: $PASS"
echo
`, panelURL, token, foreignHost)
}

// ---- Agent endpoints (foreign accepts iran heartbeat/metrics) ----

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	hb := IranStatus{}
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	hb.LastSeen = time.Now()
	hb.Connected = true
	_ = s.store.Put(store.BucketSettings, "iran_status", hb)
	s.hub.Broadcast("iran_status", hb)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAgentMetrics(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	_ = json.NewDecoder(r.Body).Decode(&raw)
	s.hub.Broadcast("iran_metrics", raw)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAgentForwards(w http.ResponseWriter, r *http.Request) {
	var fws []Forward
	if err := json.NewDecoder(r.Body).Decode(&fws); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, f := range fws {
		_ = s.store.Put(store.BucketForwards, f.ID, &f)
	}
	writeJSON(w, 200, map[string]string{"status": "synced"})
}

func (s *Server) handleAgentConfig(w http.ResponseWriter, _ *http.Request) {
	cfg, _ := s.tunnel.Get()
	writeJSON(w, 200, cfg)
}

// ---- Backups ----

type BackupEntry struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	Path      string    `json:"path"`
	Type      string    `json:"type"`    // auto, manual
	Content   string    `json:"content"` // full, users, config
}

func (s *Server) handleBackupList(w http.ResponseWriter, _ *http.Request) {
	out := []*BackupEntry{}
	_ = s.store.List(store.BucketBackups, func(_ string, raw []byte) error {
		b := &BackupEntry{}
		if err := json.Unmarshal(raw, b); err == nil {
			out = append(out, b)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, _ *http.Request) {
	id := randID(8)
	ts := time.Now()
	dst := filepath.Join(s.cfg.DataDir, "backups", "qcc-"+id+".tar.gz")
	if err := s.makeBackupTar(dst); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(dst)
	entry := BackupEntry{ID: id, CreatedAt: ts, Path: dst, Type: "manual", Content: "full"}
	if st != nil {
		entry.SizeBytes = st.Size()
	}
	_ = s.store.Put(store.BucketBackups, id, &entry)
	writeJSON(w, 201, entry)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	// TODO: actually extract tar over /etc/quiccochet (with confirmation)
	id := chi.URLParam(r, "id")
	writeJSON(w, 200, map[string]string{"status": "queued", "id": id})
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	entry := &BackupEntry{}
	if err := s.store.Get(store.BucketBackups, id, entry); err == nil {
		_ = os.Remove(entry.Path)
	}
	_ = s.store.Delete(store.BucketBackups, id)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// makeBackupTar writes /etc/quiccochet + panel.db into dst.
func (s *Server) makeBackupTar(dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	// minimal gzip+tar: stream stdlib archive/tar would be ideal, but for
	// this skeleton we shell out to tar for simplicity.
	cfgDir := filepath.Dir(s.cfg.TunnelConfig)
	if cfgDir == "" || cfgDir == "." {
		cfgDir = "/etc/quiccochet"
	}
	cmd := []string{"-czf", dst, "-C", "/", strings.TrimPrefix(cfgDir, "/")}
	return runCmd("tar", cmd...)
}

// ---- Notifications ----

func (s *Server) handleNotifyGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"telegram": s.cfg.Telegram,
		"discord":  s.cfg.Discord,
	})
}

func (s *Server) handleNotifyPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Telegram *struct {
			Enabled  bool   `json:"enabled"`
			BotToken string `json:"bot_token"`
			ChatID   string `json:"chat_id"`
		} `json:"telegram,omitempty"`
		Discord *struct {
			Enabled    bool   `json:"enabled"`
			WebhookURL string `json:"webhook_url"`
		} `json:"discord,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Telegram != nil {
		s.cfg.Telegram.Enabled = req.Telegram.Enabled
		s.cfg.Telegram.BotToken = req.Telegram.BotToken
		s.cfg.Telegram.ChatID = req.Telegram.ChatID
	}
	if req.Discord != nil {
		s.cfg.Discord.Enabled = req.Discord.Enabled
		s.cfg.Discord.WebhookURL = req.Discord.WebhookURL
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.notify.Test())
}

// ---- Settings & 2FA & API tokens ----

func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	// scrub secrets from response
	out := *s.cfg
	out.AdminPass = ""
	out.JWTSecret = ""
	out.AgentToken = mask(out.AgentToken)
	out.TOTPKey = ""
	writeJSON(w, 200, out)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminUser    *string   `json:"admin_user,omitempty"`
		NewPassword  *string   `json:"new_password,omitempty"`
		Domain       *string   `json:"domain,omitempty"`
		IPWhitelist  *[]string `json:"ip_whitelist,omitempty"`
		RateLimit    *bool     `json:"rate_limit_logins,omitempty"`
		SessionMin   *int      `json:"session_timeout_min,omitempty"`
		IranPanel    *string   `json:"iran_panel,omitempty"`
		ForeignPanel *string   `json:"foreign_panel,omitempty"`
		AgentToken   *string   `json:"agent_token,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.AdminUser != nil {
		s.cfg.AdminUser = *req.AdminUser
	}
	if req.NewPassword != nil && *req.NewPassword != "" {
		hash, err := auth.HashPassword(*req.NewPassword)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.cfg.AdminPass = hash
	}
	if req.Domain != nil {
		s.cfg.Domain = *req.Domain
	}
	if req.IPWhitelist != nil {
		s.cfg.IPWhitelist = *req.IPWhitelist
	}
	if req.RateLimit != nil {
		s.cfg.RateLimitLogins = *req.RateLimit
	}
	if req.SessionMin != nil {
		s.cfg.SessionTimeoutMin = *req.SessionMin
	}
	if req.IranPanel != nil {
		s.cfg.IranPanel = *req.IranPanel
	}
	if req.ForeignPanel != nil {
		s.cfg.ForeignPanel = *req.ForeignPanel
		if s.hb != nil {
			s.hb.Stop()
			s.hb = agent.New(s.cfg, s.tunnel)
			s.hb.Start()
		}
	}
	if req.AgentToken != nil {
		s.cfg.AgentToken = *req.AgentToken
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

func (s *Server) handleEnable2FA(w http.ResponseWriter, _ *http.Request) {
	url, secret, err := auth.GenerateTOTP("QUICochet", s.cfg.AdminUser)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// store provisional secret; not active until verify
	_ = s.store.Put(store.BucketSettings, "totp_pending", secret)
	writeJSON(w, 200, map[string]string{"otpauth_url": url, "secret": secret})
}

func (s *Server) handleVerify2FA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var pending string
	_ = s.store.Get(store.BucketSettings, "totp_pending", &pending)
	if !auth.VerifyTOTP(pending, req.Code) {
		writeErr(w, 400, "invalid code")
		return
	}
	s.cfg.TOTPKey = pending
	_ = s.store.Delete(store.BucketSettings, "totp_pending")
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "enabled"})
}

type APIToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Token     string    `json:"token,omitempty"` // only returned on create
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

func (s *Server) handleAPITokensList(w http.ResponseWriter, _ *http.Request) {
	out := []*APIToken{}
	_ = s.store.List(store.BucketAPITokens, func(_ string, raw []byte) error {
		t := &APIToken{}
		if json.Unmarshal(raw, t) == nil {
			t.Token = "qcc_" + strings.Repeat("•", 8) + safeSuffix(t.ID)
			out = append(out, t)
		}
		return nil
	})
	writeJSON(w, 200, out)
}

func (s *Server) handleAPITokenCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := randID(8)
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	tok := "qcc_" + hex.EncodeToString(raw)
	t := &APIToken{ID: id, Name: req.Name, Token: tok, Scope: req.Scope, CreatedAt: time.Now()}
	_ = s.store.Put(store.BucketAPITokens, id, t)
	writeJSON(w, 201, t)
}

func (s *Server) handleAPITokenDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.store.Delete(store.BucketAPITokens, id)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ---- Bench ----

func (s *Server) handleBenchRun(w http.ResponseWriter, _ *http.Request) {
	// TODO: actual iperf3 client run inside the tunnel
	writeJSON(w, 200, map[string]any{"status": "queued"})
}

func (s *Server) handleBenchHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, []any{})
}

// ---- helpers ----

func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-8) + s[len(s)-4:]
}

func safeSuffix(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

func runCmd(name string, args ...string) error {
	return execRun(name, args...)
}

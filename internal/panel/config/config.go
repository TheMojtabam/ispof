package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is the panel + tunnel runtime configuration.
type Config struct {
	// Side: "server" (foreign) or "client" (iran)
	Side string `json:"side,omitempty"`

	// Panel HTTP listener
	Listen    string `json:"listen"`
	BehindTLS bool   `json:"behind_tls,omitempty"`
	Domain    string `json:"domain,omitempty"`
	BasePath  string `json:"base_path,omitempty"`

	// Auth
	AdminUser string `json:"admin_user"`
	AdminPass string `json:"admin_pass"` // bcrypt hash; if missing, JWT secret will be set
	JWTSecret string `json:"jwt_secret,omitempty"`
	TOTPKey   string `json:"totp_key,omitempty"`

	// Inter-side comms
	AgentToken   string `json:"agent_token"`
	ForeignPanel string `json:"foreign_panel,omitempty"` // iran -> foreign
	IranPanel    string `json:"iran_panel,omitempty"`    // foreign -> iran (for read-only view)

	// Storage / paths
	DataDir  string `json:"data_dir"`
	LogDir   string `json:"log_dir"`
	PanelDir string `json:"panel_dir,omitempty"` // optional override for embedded UI

	// Tunnel config path managed by panel
	TunnelConfig string `json:"tunnel_config"`
	XrayConfig   string `json:"xray_config,omitempty"`

	// Notifications
	Telegram TelegramConfig `json:"telegram,omitempty"`
	Discord  DiscordConfig  `json:"discord,omitempty"`

	// Behavior toggles
	AllowSignup       bool     `json:"allow_signup,omitempty"`
	IPWhitelist       []string `json:"ip_whitelist,omitempty"`
	RateLimitLogins   bool     `json:"rate_limit_logins,omitempty"`
	SessionTimeoutMin int      `json:"session_timeout_min,omitempty"`

	loadedFrom string
}

type TelegramConfig struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}
type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
}

// Load reads JSON config from path, applying defaults.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config %s not found", path)
		}
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.loadedFrom = path
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:9999"
	}
	if c.AdminUser == "" {
		c.AdminUser = "admin"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/quiccochet"
	}
	if c.LogDir == "" {
		c.LogDir = "/var/log/quiccochet"
	}
	if c.SessionTimeoutMin == 0 {
		c.SessionTimeoutMin = 30
	}
	if c.JWTSecret == "" {
		c.JWTSecret = randHex(32)
	}
}

func (c *Config) Save() error {
	if c.loadedFrom == "" {
		return errors.New("config has no path")
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.loadedFrom + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.loadedFrom)
}

func (c *Config) Path() string { return c.loadedFrom }

func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.DataDir, c.LogDir, filepath.Join(c.DataDir, "backups"), filepath.Join(c.DataDir, "installers")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}

// Package tunnel manages the QUICochet tunnel configuration file from the panel
// side, and exposes a thin wrapper around the existing admin Unix socket so
// the web panel can fetch live stats from the running daemon.
package tunnel

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
)

// Config mirrors the JSON shape consumed by the QUICochet binary.
//
// We intentionally keep sub-blocks as raw JSON so the panel doesn't need to
// know every protocol option — when the user edits a knob in the UI, the
// frontend ships back the entire JSON tree which we persist verbatim.
type Config struct {
	Mode        string          `json:"mode"`
	Transport   json.RawMessage `json:"transport,omitempty"`
	ListenPort  int             `json:"listen_port,omitempty"`
	Server      json.RawMessage `json:"server,omitempty"`
	Spoof       json.RawMessage `json:"spoof,omitempty"`
	Crypto      json.RawMessage `json:"crypto,omitempty"`
	Performance json.RawMessage `json:"performance,omitempty"`
	QUIC        json.RawMessage `json:"quic,omitempty"`
	Obfuscation json.RawMessage `json:"obfuscation,omitempty"`
	Security    json.RawMessage `json:"security,omitempty"`
	Failover    json.RawMessage `json:"failover,omitempty"`
	Logging     json.RawMessage `json:"logging,omitempty"`
	Admin       json.RawMessage `json:"admin,omitempty"`
}

// Manager loads/saves the tunnel JSON and tracks live metrics by talking to
// the daemon's admin Unix socket.
type Manager struct {
	path      string
	adminSock string
	mu        sync.Mutex

	statsMu  sync.Mutex
	lastSnap Snapshot
	prevAt   time.Time
	prevTx   uint64
	prevRx   uint64
	history  []SamplePoint
}

// NewManager creates a panel-side tunnel manager.
//
//	path       — path to the daemon's JSON config (e.g. /etc/quiccochet/server.json)
//	adminSock  — path to the daemon's admin Unix socket (default /run/quiccochet.sock)
func NewManager(path string) (*Manager, error) {
	if path == "" {
		return nil, errors.New("tunnel: empty config path")
	}
	return &Manager{path: path, adminSock: defaultAdminSocket()}, nil
}

func defaultAdminSocket() string {
	for _, p := range []string{"/run/quiccochet.sock", "/var/run/quiccochet.sock", "/tmp/quiccochet.sock"} {
		if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
			return p
		}
	}
	return "/run/quiccochet.sock"
}

// SetAdminSocket overrides the auto-detected admin socket path.
func (m *Manager) SetAdminSocket(p string) { m.adminSock = p }

func (m *Manager) Get() (*Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Mode: "server"}, nil
		}
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (m *Manager) Save(cfg *Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// GenerateKeyPair returns base64 X25519 (priv, pub).
func GenerateKeyPair() (priv, pub string, err error) {
	p := make([]byte, 32)
	if _, err = rand.Read(p); err != nil {
		return "", "", err
	}
	p[0] &= 248
	p[31] &= 127
	p[31] |= 64
	pubBytes, err := curve25519.X25519(p, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p),
		base64.StdEncoding.EncodeToString(pubBytes), nil
}

// queryAdmin dials the daemon's admin socket and runs a single line command.
//
// The protocol matches internal/admin/admin.go in this repo: one request line
// terminated by \n, response is a single JSON line followed by EOF.
func (m *Manager) queryAdmin(cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", m.adminSock, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return line, nil
}

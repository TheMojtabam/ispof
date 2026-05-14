// Package store persists tunnel configurations as individual JSON files
// under /etc/ispof/tunnels/. Each file is a complete config that the
// underlying `quiccochet` binary can read directly with -c, so the store
// doubles as the source of truth for both the panel and the daemons.
//
// Schema compatibility: the on-disk JSON layout matches quiccochet's
// own Config struct as closely as possible. Fields the panel doesn't
// recognize are captured into Extra during Unmarshal and re-emitted on
// Marshal, so unknown / future / advanced fields are preserved when
// the panel saves a config the user originally created by hand.
//
// Concurrency: every public method takes a single RW mutex. Writes are
// rare enough that finer locking would be over-engineering.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	DefaultDir = "/etc/ispof/tunnels"
	fileMode   = 0o600 // private keys live in these files
	dirMode    = 0o750
)

// validName matches the systemd unit-instance-name character class so a
// store name maps 1:1 to a quiccochet@<name>.service unit.
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// ─────────────────────────── data model ───────────────────────────

// Tunnel is the in-memory representation of a single tunnel. It mirrors
// quiccochet's own Config struct field-for-field. Anything the panel
// doesn't have a typed field for lands in Extra and is preserved on save.
type Tunnel struct {
	// Panel metadata. quiccochet ignores these (json.Unmarshal in the
	// daemon tolerates unknown fields). We keep them in the on-disk JSON
	// so the panel can be restarted without losing track of when a
	// config was created.
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// ── fields below match quiccochet's Config struct ──
	Mode          string         `json:"mode"`
	Transport     Transport      `json:"transport"`
	ListenPort    int            `json:"listen_port,omitempty"`
	Server        *Server        `json:"server,omitempty"`
	Spoof         Spoof          `json:"spoof,omitempty"`
	Crypto        Crypto         `json:"crypto,omitempty"`
	Performance   Performance    `json:"performance,omitempty"`
	Obfuscation   Obfuscation    `json:"obfuscation,omitempty"`
	QUIC          QUIC           `json:"quic,omitempty"`
	Security      Security       `json:"security,omitempty"`
	OutboundProxy *OutboundProxy `json:"outbound_proxy,omitempty"`
	Logging       Logging        `json:"logging,omitempty"`
	Admin         Admin          `json:"admin,omitempty"`
	Metrics       Metrics        `json:"metrics,omitempty"`
	Inbounds      []Inbound      `json:"inbounds,omitempty"`
	Peers         []Peer         `json:"peers,omitempty"`

	// ImportedFrom records the absolute filesystem path the config was
	// imported from via /api/discover/import. The panel uses this on
	// delete to also remove the source file — without it, deleting a
	// tunnel from the UI would leave the original on disk and the next
	// discovery scan would surface it again, looking like the delete
	// "didn't work". The leading underscore marks it as panel-internal
	// metadata; quiccochet silently ignores unknown fields.
	ImportedFrom string `json:"_ispof_imported_from,omitempty"`

	// Extra captures any JSON keys we don't have a typed field for.
	// They are re-emitted on Marshal so users can keep custom keys in
	// their configs without the panel stomping on them.
	Extra map[string]json.RawMessage `json:"-"`
}

type Transport struct {
	Type           string `json:"type"`
	ICMPMode       string `json:"icmp_mode,omitempty"`
	ProtocolNumber int    `json:"protocol_number,omitempty"`
}

type Server struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type Spoof struct {
	SourceIPs    []string `json:"source_ips,omitempty"`
	SourceIPv6s  []string `json:"source_ipv6s,omitempty"`
	PeerSpoofIPs []string `json:"peer_spoof_ips,omitempty"`
	PeerSpoofV6s []string `json:"peer_spoof_ipv6s,omitempty"`
}

type Crypto struct {
	// PrivateKey is the X25519 private key in base64. quiccochet reads
	// it directly from this field — there is no separate key file.
	PrivateKey    string `json:"private_key,omitempty"`
	PeerPublicKey string `json:"peer_public_key,omitempty"`
}

type Peer struct {
	Name           string   `json:"name"`
	PeerPublicKey  string   `json:"peer_public_key"`
	ClientRealIP   string   `json:"client_real_ip,omitempty"`
	ClientRealV6   string   `json:"client_real_ipv6,omitempty"`
	SourceIPs      []string `json:"source_ips,omitempty"`
	SourceIPv6s    []string `json:"source_ipv6s,omitempty"`
	PeerSpoofIPs   []string `json:"peer_spoof_ips,omitempty"`
	PeerSpoofIPv6s []string `json:"peer_spoof_ipv6s,omitempty"`
}

type Performance struct {
	BufferSize     int `json:"buffer_size,omitempty"`
	MTU            int `json:"mtu,omitempty"`
	ReadBuffer     int `json:"read_buffer,omitempty"`
	WriteBuffer    int `json:"write_buffer,omitempty"`
	JitterBufferMs int `json:"jitter_buffer_ms,omitempty"`
	PacingRateMbps int `json:"pacing_rate_mbps,omitempty"`
}

type QUIC struct {
	KeepAlivePeriodSec         int    `json:"keep_alive_period_sec,omitempty"`
	MaxIdleTimeoutSec          int    `json:"max_idle_timeout_sec,omitempty"`
	MaxStreamReceiveWindow     int    `json:"max_stream_receive_window,omitempty"`
	MaxConnectionReceiveWindow int    `json:"max_connection_receive_window,omitempty"`
	MaxIncomingStreams         int    `json:"max_incoming_streams,omitempty"`
	MaxConcurrentSessions      int    `json:"max_concurrent_sessions,omitempty"`
	PoolSize                   int    `json:"pool_size,omitempty"`
	UDPRouteIdleSec            int    `json:"udp_route_idle_sec,omitempty"`
	UDPRouteMax                int    `json:"udp_route_max,omitempty"`
	CongestionControl          string `json:"congestion_control,omitempty"`
	PacketThreshold            int    `json:"packet_threshold,omitempty"`
	StreamCloseTimeoutSec      int    `json:"stream_close_timeout_sec,omitempty"`
}

type Obfuscation struct {
	Mode               string `json:"mode,omitempty"`
	ChaffingIntervalMs int    `json:"chaffing_interval_ms,omitempty"`
}

type Security struct {
	BlockPrivateTargets bool `json:"block_private_targets"`
}

// OutboundProxy is top-level in quiccochet's schema — we mirror that.
type OutboundProxy struct {
	Enabled  bool   `json:"enabled"`
	Type     string `json:"type"`
	Address  string `json:"address"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type Inbound struct {
	Type         string         `json:"type"`
	Listen       string         `json:"listen"`
	AuthEnabled  bool           `json:"auth_enabled,omitempty"`
	AuthUsers    []InboundAuthU `json:"auth_users,omitempty"`
	UpstreamHost string         `json:"upstream_host,omitempty"`
	UpstreamPort int            `json:"upstream_port,omitempty"`
}

type InboundAuthU struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Logging struct {
	Level      string `json:"level,omitempty"`
	File       string `json:"file,omitempty"`
	Statistics bool   `json:"statistics,omitempty"`
}

type Admin struct {
	Enabled bool   `json:"enabled"`
	Socket  string `json:"socket,omitempty"`
}

type Metrics struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen,omitempty"`
}

// ─────────────────────────── marshal/unmarshal with Extra preservation ───────────────────────────

// UnmarshalJSON splits the input into known fields (the struct) and
// unknown ones (Extra). This lets users add fields the panel doesn't
// know about — e.g. new quiccochet options — without them being
// silently dropped on save.
func (t *Tunnel) UnmarshalJSON(data []byte) error {
	// First pass: standard parse into the struct.
	type alias Tunnel
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = Tunnel(a)

	// Second pass: parse into a map and find keys that weren't claimed
	// by typed fields.
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	known := knownTunnelKeys()
	t.Extra = make(map[string]json.RawMessage)
	for k, v := range all {
		if !known[k] {
			t.Extra[k] = v
		}
	}
	if len(t.Extra) == 0 {
		t.Extra = nil
	}
	return nil
}

// MarshalJSON emits the struct fields followed by the Extra map so
// unknown fields the panel preserved round-trip cleanly.
func (t Tunnel) MarshalJSON() ([]byte, error) {
	type alias Tunnel
	a := alias(t)
	a.Extra = nil // we'll merge Extra manually
	primary, err := json.Marshal(&a)
	if err != nil {
		return nil, err
	}
	if len(t.Extra) == 0 {
		return primary, nil
	}
	// Re-parse to a map, overlay Extra, re-emit. This is the simplest
	// way to merge while preserving key order behaviour from
	// encoding/json (sorted, which is fine for our use case).
	var combined map[string]json.RawMessage
	if err := json.Unmarshal(primary, &combined); err != nil {
		return nil, err
	}
	for k, v := range t.Extra {
		if _, taken := combined[k]; !taken {
			combined[k] = v
		}
	}
	return json.Marshal(combined)
}

// knownTunnelKeys is the set of JSON field names the typed Tunnel covers.
// Used to decide which keys go to Extra during UnmarshalJSON.
func knownTunnelKeys() map[string]bool {
	return map[string]bool{
		"name": true, "created_at": true, "updated_at": true,
		"mode": true, "transport": true, "listen_port": true, "server": true,
		"spoof": true, "crypto": true, "performance": true, "obfuscation": true,
		"quic": true, "security": true, "outbound_proxy": true, "logging": true,
		"admin": true, "metrics": true, "inbounds": true, "peers": true,
		"_ispof_imported_from": true,
	}
}

// ─────────────────────────── store ───────────────────────────

type Store struct {
	dir string
	mu  sync.RWMutex

	// Tiny in-memory cache to avoid re-reading every file on every SSE
	// tick. Invalidated whenever a write happens.
	cache       []Tunnel
	cacheDir    string
	cacheMtime  time.Time
	cacheStale  bool
	cacheNeedReload bool
}

func New(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Store{dir: dir, cacheNeedReload: true}, nil
}

func (s *Store) Dir() string { return s.dir }

// List returns every tunnel in the directory, sorted by name.
// Re-reads from disk only if a write has occurred since last call,
// since otherwise the cache is valid (we own the directory).
func (s *Store) List() ([]Tunnel, error) {
	s.mu.RLock()
	if !s.cacheNeedReload && s.cache != nil {
		out := s.cache
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	// Need to reload — take write lock to update cache.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-check under write lock (someone may have populated while we
	// waited).
	if !s.cacheNeedReload && s.cache != nil {
		return s.cache, nil
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.dir, err)
	}
	out := make([]Tunnel, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t, err := s.loadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue // skip malformed configs rather than fail the whole list
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	s.cache = out
	s.cacheNeedReload = false
	return out, nil
}

func (s *Store) Get(name string) (Tunnel, error) {
	if !validName.MatchString(name) {
		return Tunnel{}, ErrInvalidName
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadFile(s.path(name))
}

func (s *Store) Create(t Tunnel) (Tunnel, error) {
	if err := validateTunnel(t); err != nil {
		return Tunnel{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path(t.Name)); err == nil {
		return Tunnel{}, ErrAlreadyExists
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if err := s.write(t); err != nil {
		return Tunnel{}, err
	}
	s.invalidateCache()
	return t, nil
}

func (s *Store) Update(t Tunnel) (Tunnel, error) {
	if err := validateTunnel(t); err != nil {
		return Tunnel{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.loadFile(s.path(t.Name))
	if err != nil {
		return Tunnel{}, err
	}
	t.CreatedAt = existing.CreatedAt
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.UpdatedAt = time.Now().UTC()
	// Preserve Extra from existing if caller didn't pass any.
	if t.Extra == nil && existing.Extra != nil {
		t.Extra = existing.Extra
	}
	if err := s.write(t); err != nil {
		return Tunnel{}, err
	}
	s.invalidateCache()
	return t, nil
}

func (s *Store) Delete(name string) error {
	if !validName.MatchString(name) {
		return ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.invalidateCache()
	return nil
}

// Import accepts a fully-formed Tunnel (typically discovered from
// somewhere else on the server) and writes it into the store under
// the given target name. Fails if the name already exists.
func (s *Store) Import(name string, t Tunnel) (Tunnel, error) {
	if !validName.MatchString(name) {
		return Tunnel{}, ErrInvalidName
	}
	t.Name = name
	return s.Create(t)
}

// InvalidateCache forces the next List() to re-read from disk. Useful
// when an out-of-band write happens (e.g. the user dropped a config
// file in by hand).
func (s *Store) InvalidateCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCache()
}

func (s *Store) invalidateCache() { s.cacheNeedReload = true }

// ─────────────────────────── internals ───────────────────────────

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

func (s *Store) loadFile(p string) (Tunnel, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Tunnel{}, ErrNotFound
		}
		return Tunnel{}, fmt.Errorf("read %s: %w", p, err)
	}
	var t Tunnel
	if err := json.Unmarshal(data, &t); err != nil {
		return Tunnel{}, fmt.Errorf("parse %s: %w", p, err)
	}
	// If the file has no embedded name (e.g. it came from an external
	// quiccochet config that didn't include one), derive from filename.
	if t.Name == "" {
		base := filepath.Base(p)
		t.Name = base[:len(base)-len(filepath.Ext(base))]
	}
	return t, nil
}

func (s *Store) write(t Tunnel) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	final := s.path(t.Name)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return os.Rename(tmp.Name(), final)
}

func validateTunnel(t Tunnel) error {
	if !validName.MatchString(t.Name) {
		return ErrInvalidName
	}
	switch t.Mode {
	case "client", "server":
	default:
		return fmt.Errorf("invalid mode %q (want client|server)", t.Mode)
	}
	switch t.Transport.Type {
	case "udp", "icmp", "icmpv6", "raw", "syn_udp":
	default:
		return fmt.Errorf("invalid transport %q", t.Transport.Type)
	}
	if t.Transport.Type == "raw" {
		if t.Transport.ProtocolNumber < 1 || t.Transport.ProtocolNumber > 255 {
			return fmt.Errorf("raw transport requires protocol_number 1-255")
		}
	}
	if t.Mode == "client" && t.Server == nil {
		return fmt.Errorf("client mode requires server.{address,port}")
	}
	return nil
}

var (
	ErrNotFound      = errors.New("tunnel not found")
	ErrAlreadyExists = errors.New("tunnel already exists")
	ErrInvalidName   = errors.New("invalid tunnel name (must be alphanumeric/_.-, ≤63 chars)")
)

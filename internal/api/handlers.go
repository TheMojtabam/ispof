// Package api wires HTTP routes onto store + procmgr + scraper + events
// + discover. State results from systemd are cached briefly to avoid
// hammering systemctl from every SSE tick × every browser tab.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheMojtabam/ispof/internal/cryptoutil"
	"github.com/TheMojtabam/ispof/internal/discover"
	"github.com/TheMojtabam/ispof/internal/events"
	"github.com/TheMojtabam/ispof/internal/procmgr"
	"github.com/TheMojtabam/ispof/internal/scraper"
	"github.com/TheMojtabam/ispof/internal/store"
)

// stateCacheTTL is how long we hold each tunnel's systemd state before
// re-running `systemctl show`. SSE ticks at ~5s but multiple tabs each
// open their own stream, so even a 1-second cache cuts systemctl forks
// by N (tabs).
const stateCacheTTL = 1500 * time.Millisecond

type Server struct {
	store   *store.Store
	proc    *procmgr.Manager
	scrape  *scraper.Scraper
	events  *events.Log
	version string
	commit  string
	built   string

	stateMu   sync.Mutex
	stateCache map[string]cachedState

	lastSeenMu sync.Mutex
	lastSeen   map[string]string // for state-change detection
}

type cachedState struct {
	state procmgr.State
	at    time.Time
}

type Deps struct {
	Store   *store.Store
	Proc    *procmgr.Manager
	Scraper *scraper.Scraper
	Events  *events.Log
	Version string
	Commit  string
	Built   string
}

func New(d Deps) *Server {
	return &Server{
		store:      d.Store,
		proc:       d.Proc,
		scrape:     d.Scraper,
		events:     d.Events,
		version:    d.Version,
		commit:     d.Commit,
		built:      d.Built,
		stateCache: make(map[string]cachedState),
		lastSeen:   make(map[string]string),
	}
}

// stateFor returns the systemd state for a tunnel, using the cache when
// the entry is fresher than stateCacheTTL. The point is to absorb the
// fan-out from N SSE listeners.
//
// We also probe for "externally running" — a quiccochet process matching
// this tunnel that lives outside our template unit — so the UI can
// surface "tunnel is running, but not via the panel". This is the case
// users hit right after import: their pre-existing quiccochet process is
// still running, and the panel's template unit reports inactive.
func (s *Server) stateFor(name string) procmgr.State {
	s.stateMu.Lock()
	if c, ok := s.stateCache[name]; ok && time.Since(c.at) < stateCacheTTL {
		st := c.state
		s.stateMu.Unlock()
		return st
	}
	s.stateMu.Unlock()

	st, err := s.proc.State(name)
	if err != nil {
		st = procmgr.State{Name: name}
	}

	// If systemd says inactive, look for an external process before
	// caching. (If systemd already says active, the panel is the source
	// of truth — no need to also probe `ps`.)
	if st.ActiveState != "active" {
		configPath := s.store.Dir() + "/" + name + ".json"
		if pid, cmd := s.proc.FindExternal(context.Background(), name, configPath); pid > 0 {
			st.ExternalPid = pid
			st.ExternalCmd = cmd
		}
	}

	s.stateMu.Lock()
	s.stateCache[name] = cachedState{state: st, at: time.Now()}
	s.stateMu.Unlock()
	return st
}

// invalidateState clears any cached state AND any state-change detector
// memory for a tunnel. Called when a tunnel is deleted so we don't
// accumulate entries for tunnels that no longer exist.
func (s *Server) invalidateState(name string) {
	s.stateMu.Lock()
	delete(s.stateCache, name)
	s.stateMu.Unlock()
	s.lastSeenMu.Lock()
	delete(s.lastSeen, name)
	s.lastSeenMu.Unlock()
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/system", s.handleSystem)
	mux.HandleFunc("/api/keygen", s.handleKeygen)
	mux.HandleFunc("/api/events", s.handleEvents)

	mux.HandleFunc("/api/tunnels", s.handleTunnels)
	mux.HandleFunc("/api/tunnels/", s.handleTunnelsByName)

	mux.HandleFunc("/api/discover", s.handleDiscover)
	mux.HandleFunc("/api/discover/import", s.handleDiscoverImport)

	mux.HandleFunc("/api/stream/state", s.handleStreamState)
}

// ─────────────────────────── basic info ───────────────────────────

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"version": s.version, "commit": s.commit, "built": s.built,
		"go_runtime": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
	})
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	writeJSON(w, 200, map[string]any{
		"hostname":    hostname,
		"goroutines":  runtime.NumGoroutine(),
		"tunnels_dir": s.store.Dir(),
		"go_runtime":  runtime.Version(),
		"now_unix":    time.Now().Unix(),
	})
}

func (s *Server) handleKeygen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kp, err := cryptoutil.GenerateKeyPair()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, kp)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n == 0 {
		n = 50
	}
	writeJSON(w, 200, map[string]any{"events": s.events.Recent(n)})
}

// ─────────────────────────── discover ───────────────────────────

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	hits := discover.Discover(ctx, discover.Options{ExcludeDir: s.store.Dir()})
	writeJSON(w, 200, map[string]any{"hits": hits})
}

// handleDiscoverImport copies a discovered config into the store. The
// body is {paths: [...], target_names: {path: name, ...}} where the
// optional target_names lets the UI override the auto-derived name
// (e.g. when two configs would collide).
func (s *Server) handleDiscoverImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Paths       []string          `json:"paths"`
		TargetNames map[string]string `json:"target_names,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	imported := []string{}
	failed := map[string]string{}
	for _, p := range req.Paths {
		// Security gate: only allow importing paths that look like
		// quiccochet configs. Otherwise the panel becomes an arbitrary
		// file-read primitive — a caller with auth could `import` any
		// path (/etc/shadow, /root/.ssh/id_rsa, ...) and observe the
		// resulting parse error which can leak file contents.
		if !discover.LooksLikeQuiccochetConfig(p) {
			failed[p] = "refused: path does not contain a quiccochet config (missing transport/mode/spoof markers)"
			continue
		}
		data, err := discover.ReadConfig(p)
		if err != nil {
			failed[p] = err.Error()
			continue
		}
		var t store.Tunnel
		if err := json.Unmarshal(data, &t); err != nil {
			failed[p] = "parse: " + err.Error()
			continue
		}
		name := req.TargetNames[p]
		if name == "" {
			name = t.Name
		}
		if name == "" {
			// derive from filename
			name = strings.TrimSuffix(pathBase(p), ".json")
		}
		t.Name = name
		// Track the source path so a later delete can also remove the
		// original file. Without this the user's mental model breaks —
		// they "delete" a tunnel, refresh discover, see it come right
		// back.
		t.ImportedFrom = p
		saved, err := s.store.Create(t)
		if err != nil {
			failed[p] = err.Error()
			continue
		}
		imported = append(imported, saved.Name)
		s.events.Push(events.Event{
			Type: "imported", Level: events.Info, Tunnel: saved.Name,
			Message: "imported from " + p,
		})
	}
	writeJSON(w, 200, map[string]any{"imported": imported, "failed": failed})
}

// pathBase is filepath.Base, inlined to avoid importing path/filepath
// just for one call in this file's scope.
func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ─────────────────────────── tunnel routes ───────────────────────────

func (s *Server) handleTunnels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTunnels(w, r)
	case http.MethodPost:
		s.createTunnel(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTunnelsByName(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/tunnels/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.getTunnel(w, r, name)
		case http.MethodPut:
			s.updateTunnel(w, r, name)
		case http.MethodDelete:
			s.deleteTunnel(w, r, name)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	switch parts[1] {
	case "start", "stop", "restart", "enable", "disable":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.tunnelAction(w, r, name, parts[1])
	case "logs":
		s.tunnelLogs(w, r, name)
	case "state":
		s.tunnelState(w, r, name)
	case "metrics":
		s.tunnelMetrics(w, r, name)
	case "history":
		s.tunnelHistory(w, r, name)
	case "stream":
		if len(parts) > 2 && parts[2] == "logs" {
			s.tunnelStreamLogs(w, r, name)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// fullView is the API response shape: stored config + live state + scrape.
// Scrape is named so to avoid colliding with the embedded Tunnel.Metrics
// (which is the config block).
//
// We override MarshalJSON because Go's encoding/json silently drops
// outer fields (State, Scrape) when the embedded type (Tunnel) defines
// its own MarshalJSON — the embedded method "promotes" and takes over
// the whole marshaling. We work around this by marshaling the Tunnel
// to a map and overlaying our additional fields manually.
type fullView struct {
	store.Tunnel
	State  procmgr.State   `json:"state"`
	Scrape *scraper.Sample `json:"scrape,omitempty"`
}

func (v fullView) MarshalJSON() ([]byte, error) {
	// Marshal the embedded Tunnel (uses its custom MarshalJSON which
	// folds the Extra map back in).
	tb, err := json.Marshal(v.Tunnel)
	if err != nil {
		return nil, err
	}
	// Re-parse to a map so we can overlay our outer fields.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(tb, &m); err != nil {
		return nil, err
	}
	stateB, err := json.Marshal(v.State)
	if err != nil {
		return nil, err
	}
	m["state"] = stateB
	if v.Scrape != nil {
		scrapeB, err := json.Marshal(v.Scrape)
		if err != nil {
			return nil, err
		}
		m["scrape"] = scrapeB
	}
	return json.Marshal(m)
}

func metricsTarget(t store.Tunnel) string {
	if t.Metrics.Listen == "" {
		return ""
	}
	return "http://" + t.Metrics.Listen + "/metrics"
}

func (s *Server) ScraperTargets() map[string]string {
	tunnels, err := s.store.List()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(tunnels))
	for _, t := range tunnels {
		out[t.Name] = metricsTarget(t)
	}
	return out
}

// DetectStateChanges polls systemd state for each tunnel and emits an
// event whenever the active state changes. Runs on a separate timer in
// main.go from the scraper so state changes propagate faster than the
// scrape interval.
func (s *Server) DetectStateChanges() {
	tunnels, err := s.store.List()
	if err != nil {
		return
	}
	for _, t := range tunnels {
		st := s.stateFor(t.Name)
		curr := st.ActiveState
		if curr == "" {
			curr = "inactive"
		}
		s.lastSeenMu.Lock()
		prev := s.lastSeen[t.Name]
		if prev != "" && prev != curr {
			s.events.Push(events.StateChange(t.Name, prev, curr))
		}
		s.lastSeen[t.Name] = curr
		s.lastSeenMu.Unlock()
	}
}

func (s *Server) listTunnels(w http.ResponseWriter, r *http.Request) {
	tunnels, err := s.store.List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	views := make([]fullView, 0, len(tunnels))
	for _, t := range tunnels {
		v := fullView{Tunnel: t, State: s.stateFor(t.Name)}
		if sample, ok := s.scrape.Latest(t.Name); ok {
			v.Scrape = &sample
		}
		views = append(views, v)
	}
	writeJSON(w, 200, map[string]any{"tunnels": views})
}

func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request, name string) {
	t, err := s.store.Get(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, 500, err)
		return
	}
	v := fullView{Tunnel: t, State: s.stateFor(name)}
	if sample, ok := s.scrape.Latest(name); ok {
		v.Scrape = &sample
	}
	writeJSON(w, 200, v)
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	var t store.Tunnel
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, 400, fmt.Errorf("parse body: %w", err))
		return
	}
	saved, err := s.store.Create(t)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			writeErr(w, 409, err)
		case errors.Is(err, store.ErrInvalidName):
			writeErr(w, 400, err)
		default:
			writeErr(w, 400, err)
		}
		return
	}
	s.events.Push(events.Created(saved.Name, saved.Mode, saved.Transport.Type))
	writeJSON(w, 201, saved)
}

func (s *Server) updateTunnel(w http.ResponseWriter, r *http.Request, name string) {
	var t store.Tunnel
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, 400, fmt.Errorf("parse body: %w", err))
		return
	}
	if t.Name != name {
		writeErr(w, 400, fmt.Errorf("name in body (%q) does not match URL (%q)", t.Name, name))
		return
	}
	saved, err := s.store.Update(t)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, 400, err)
		return
	}
	s.events.Push(events.Updated(saved.Name))
	writeJSON(w, 200, saved)
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request, name string) {
	// Look up the tunnel BEFORE deleting it so we can find the import
	// source path (if any) — once Delete returns, that record is gone.
	tunnel, lookupErr := s.store.Get(name)

	_ = s.proc.Stop(name)
	_ = s.proc.Disable(name)
	if err := s.store.Delete(name); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.invalidateState(name)

	// If this tunnel was imported from somewhere outside the store
	// (e.g. /root/my-vpn.json brought in via the discover modal),
	// remove that file too. Otherwise next discovery surfaces the same
	// config and the user thinks delete didn't work. Errors are logged
	// but not surfaced — the tunnel IS deleted from the panel even if
	// the original is missing/readonly.
	sourceMsg := ""
	if lookupErr == nil && tunnel.ImportedFrom != "" {
		// Sanity: don't try to delete anything inside the panel's own
		// tunnels dir (would be a double-free of sorts).
		src := tunnel.ImportedFrom
		if !strings.HasPrefix(src, s.store.Dir()+"/") && src != s.store.Dir() {
			if err := os.Remove(src); err == nil {
				sourceMsg = " (and source file " + src + ")"
			} else if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("could not remove import source",
					slog.String("path", src),
					slog.Any("err", err))
				sourceMsg = " (source file " + src + " could not be removed: " + err.Error() + ")"
			}
		}
	}
	s.events.Push(events.Event{
		Type: "deleted", Level: events.Info, Tunnel: name,
		Message: "tunnel deleted" + sourceMsg,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tunnelAction(w http.ResponseWriter, r *http.Request, name, action string) {
	if _, err := s.store.Get(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, 500, err)
		return
	}
	var err error
	switch action {
	case "start":
		err = s.proc.Start(name)
		if err == nil {
			s.events.Push(events.Started(name))
		}
	case "stop":
		err = s.proc.Stop(name)
		if err == nil {
			s.events.Push(events.Stopped(name))
		}
	case "restart":
		err = s.proc.Restart(name)
		if err == nil {
			s.events.Push(events.Restarted(name))
		}
	case "enable":
		err = s.proc.Enable(name)
	case "disable":
		err = s.proc.Disable(name)
	}
	s.invalidateState(name)
	if err != nil {
		s.events.Push(events.Failed(name, err.Error()))
		writeErr(w, 500, err)
		return
	}
	time.Sleep(200 * time.Millisecond)
	st := s.stateFor(name)
	writeJSON(w, 200, map[string]any{"action": action, "state": st})
}

func (s *Server) tunnelLogs(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines == 0 {
		lines = 200
	}
	out, err := s.proc.Logs(name, lines)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

func (s *Server) tunnelState(w http.ResponseWriter, r *http.Request, name string) {
	writeJSON(w, 200, s.stateFor(name))
}

func (s *Server) tunnelMetrics(w http.ResponseWriter, r *http.Request, name string) {
	sample, ok := s.scrape.Latest(name)
	if !ok {
		writeJSON(w, 200, map[string]any{"up": 0, "error": "no scrape data yet"})
		return
	}
	writeJSON(w, 200, sample)
}

func (s *Server) tunnelHistory(w http.ResponseWriter, r *http.Request, name string) {
	writeJSON(w, 200, map[string]any{"samples": s.scrape.History(name)})
}

// ─────────────────────────── streaming ───────────────────────────

// SSE state stream tick. 5s (was 2s) cuts CPU + network meaningfully
// with no perceptible difference in the UI thanks to client-side
// interpolation.
const ssePulseInterval = 5 * time.Second

type statePayload struct {
	TS      int64          `json:"ts"`
	Tunnels []fullView     `json:"tunnels"`
	Events  []events.Event `json:"events"`
}

func (s *Server) handleStreamState(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	ticker := time.NewTicker(ssePulseInterval)
	defer ticker.Stop()
	evCh := s.events.Subscribe(32)
	defer s.events.Unsubscribe(evCh)

	send := func() {
		tunnels, err := s.store.List()
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return
		}
		views := make([]fullView, 0, len(tunnels))
		for _, t := range tunnels {
			v := fullView{Tunnel: t, State: s.stateFor(t.Name)}
			if sample, ok := s.scrape.Latest(t.Name); ok {
				v.Scrape = &sample
			}
			views = append(views, v)
		}
		payload := statePayload{
			TS: time.Now().Unix(), Tunnels: views, Events: s.events.Recent(20),
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
		flusher.Flush()
	}

	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		case ev := <-evCh:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", b)
			flusher.Flush()
		}
	}
}

func (s *Server) tunnelStreamLogs(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.store.Get(name); err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd, err := s.proc.TailLogs(ctx, name)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	buf := make([]byte, 4096)
	var leftover []byte
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			data := append(leftover, buf[:n]...)
			leftover = nil
			for {
				idx := indexOfByte(data, '\n')
				if idx < 0 {
					leftover = data
					break
				}
				line := string(data[:idx])
				data = data[idx+1:]
				fmt.Fprintf(w, "event: line\ndata: %s\n\n", sseEscape(line))
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
}

// ─────────────────────────── helpers ───────────────────────────

func indexOfByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func sseEscape(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	slog.Warn("api error", slog.Int("status", status), slog.String("err", err.Error()))
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

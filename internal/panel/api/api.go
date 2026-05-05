// Package api wires HTTP routes, middlewares, and serves the embedded panel UI.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/pechenyeru/quiccochet/internal/panel/agent"
	"github.com/pechenyeru/quiccochet/internal/panel/auth"
	"github.com/pechenyeru/quiccochet/internal/panel/config"
	"github.com/pechenyeru/quiccochet/internal/panel/notify"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
	"github.com/pechenyeru/quiccochet/internal/panel/sys"
	"github.com/pechenyeru/quiccochet/internal/panel/tunnel"
	"github.com/pechenyeru/quiccochet/internal/panel/ws"
	"github.com/pechenyeru/quiccochet/web"
)

// Server wraps the http.Server + dependencies.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	hub     *ws.Hub
	tunnel  *tunnel.Manager
	notify  *notify.Dispatcher
	sysctl  *sys.Sysctl
	limiter *auth.LoginLimiter
	hb      *agent.Heartbeater
	httpSrv *http.Server
}

// NewServer wires deps and builds the chi router.
func NewServer(cfg *config.Config) (*Server, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	hub := ws.NewHub()
	go hub.Run()

	tm, err := tunnel.NewManager(cfg.TunnelConfig)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		store:   st,
		hub:     hub,
		tunnel:  tm,
		notify:  notify.NewDispatcher(cfg),
		sysctl:  sys.NewSysctl(),
		limiter: auth.NewLoginLimiter(5, 5*time.Minute),
	}
	s.hb = agent.New(cfg, tm)
	s.hb.Start()

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(s.ipWhitelist)

	// UI (no auth) — single-page HTML, CSS, and font
	r.Get("/", s.handleRoot)
	r.Get("/server", s.handleServerHTML)
	r.Get("/client", s.handleClientHTML)
	r.Get("/panel-base.css", s.handleCSS)
	r.Get("/arad-font.css", s.handleFont)

	// Public endpoints
	r.Post("/api/login", s.handleLogin)
	r.Get("/api/health", s.handleHealth)

	// Authenticated API
	r.Route("/api", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/me", s.handleMe)
		r.Get("/dashboard", s.handleDashboard)

		// Tunnel
		r.Get("/tunnel/config", s.handleTunnelGet)
		r.Put("/tunnel/config", s.handleTunnelPut)
		r.Post("/tunnel/restart", s.handleTunnelRestart)
		r.Post("/tunnel/stop", s.handleTunnelStop)
		r.Post("/tunnel/start", s.handleTunnelStart)
		r.Post("/tunnel/genkey", s.handleGenKey)

		// Iran link (only on foreign side)
		r.Get("/iran/status", s.handleIranStatus)
		r.Post("/iran/sync", s.handleIranSync)
		r.Post("/iran/push-keys", s.handleIranPushKeys)
		r.Post("/iran/installer", s.handleIranInstaller)
		r.Post("/iran/test-foreign", s.handleIranTestForeign)

		// Inbounds
		r.Get("/inbounds", s.handleInboundsList)
		r.Post("/inbounds", s.handleInboundCreate)
		r.Get("/inbounds/{id}", s.handleInboundGet)
		r.Put("/inbounds/{id}", s.handleInboundUpdate)
		r.Delete("/inbounds/{id}", s.handleInboundDelete)

		// Users
		r.Get("/users", s.handleUsersList)
		r.Post("/users", s.handleUserCreate)
		r.Get("/users/{id}", s.handleUserGet)
		r.Put("/users/{id}", s.handleUserUpdate)
		r.Delete("/users/{id}", s.handleUserDelete)
		r.Post("/users/{id}/reset-uuid", s.handleUserResetUUID)
		r.Get("/users/export.csv", s.handleUsersCSV)

		// Subscription / QR
		r.Get("/sub/{user}", s.handleSubscription)
		r.Get("/sub/{user}/qr", s.handleQR)

		// Routing
		r.Get("/routing/rules", s.handleRoutingList)
		r.Post("/routing/rules", s.handleRoutingCreate)
		r.Delete("/routing/rules/{id}", s.handleRoutingDelete)
		r.Post("/routing/geo/update", s.handleGeoUpdate)

		// Port forwards (iran side)
		r.Get("/forwards", s.handleForwardsList)
		r.Post("/forwards", s.handleForwardCreate)
		r.Delete("/forwards/{id}", s.handleForwardDelete)

		// System
		r.Get("/sys/info", s.handleSysInfo)
		r.Get("/sys/sysctl", s.handleSysctlGet)
		r.Put("/sys/sysctl", s.handleSysctlApply)
		r.Get("/sys/firewall", s.handleFirewallGet)
		r.Post("/sys/firewall/rule", s.handleFirewallAdd)

		// Service control
		r.Post("/service/{name}/{action}", s.handleServiceCtl)

		// Logs
		r.Get("/logs/tail", s.handleLogTail)

		// Metrics
		r.Get("/metrics/timeseries", s.handleMetricsTS)

		// Bench
		r.Post("/bench/run", s.handleBenchRun)
		r.Get("/bench/history", s.handleBenchHistory)

		// Backups
		r.Get("/backups", s.handleBackupList)
		r.Post("/backups", s.handleBackupCreate)
		r.Post("/backups/{id}/restore", s.handleBackupRestore)
		r.Delete("/backups/{id}", s.handleBackupDelete)

		// Notifications
		r.Get("/notify/settings", s.handleNotifyGet)
		r.Put("/notify/settings", s.handleNotifyPut)
		r.Post("/notify/test", s.handleNotifyTest)

		// Settings (panel)
		r.Get("/settings", s.handleSettingsGet)
		r.Put("/settings", s.handleSettingsPut)
		r.Post("/settings/2fa/enable", s.handleEnable2FA)
		r.Post("/settings/2fa/verify", s.handleVerify2FA)
		r.Get("/settings/api-tokens", s.handleAPITokensList)
		r.Post("/settings/api-tokens", s.handleAPITokenCreate)
		r.Delete("/settings/api-tokens/{id}", s.handleAPITokenDelete)
	})

	// Iran-side public installer route (signed)
	r.Get("/installer/{name}", s.handleInstallerDownload)

	// Live websocket
	r.Get("/ws", s.handleWS)

	// Agent endpoints (foreign accepts iran-agent push)
	r.Route("/agent", func(r chi.Router) {
		r.Use(s.requireAgent)
		r.Post("/heartbeat", s.handleAgentHeartbeat)
		r.Post("/metrics", s.handleAgentMetrics)
		r.Post("/forwards", s.handleAgentForwards)
		r.Get("/config", s.handleAgentConfig)
	})

	s.httpSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           r,
		ReadHeaderTimeout: 8 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

func (s *Server) ListenAndServe() error {
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.hb != nil {
		s.hb.Stop()
	}
	_ = s.store.Close()
	return s.httpSrv.Shutdown(ctx)
}

// ---- Static UI handlers ----

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Side == "client" {
		s.handleClientHTML(w, r)
	} else {
		s.handleServerHTML(w, r)
	}
}

func (s *Server) handleServerHTML(w http.ResponseWriter, _ *http.Request) {
	b, err := web.Server()
	if err != nil {
		http.Error(w, "ui not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleClientHTML(w http.ResponseWriter, _ *http.Request) {
	b, err := web.Client()
	if err != nil {
		http.Error(w, "ui not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleCSS(w http.ResponseWriter, _ *http.Request) {
	b, _ := web.CSS()
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (s *Server) handleFont(w http.ResponseWriter, _ *http.Request) {
	b, _ := web.Font()
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "side": s.cfg.Side, "ts": time.Now().Unix()})
}

// ---- Helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > -1 {
		host = host[:i]
	}
	return host
}

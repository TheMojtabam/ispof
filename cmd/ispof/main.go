// Command ispof runs the Ispof admin panel.
//
// The binary is self-contained: HTML/CSS/JS are embedded, tunnel configs
// live in /etc/ispof/tunnels/, and tunnel lifecycle is driven through
// systemd template units (quiccochet@.service). The underlying tunnel
// daemon (quiccochet) must be installed separately — Ispof manages
// configurations, processes, keys, metrics scraping, and logs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/TheMojtabam/ispof/internal/api"
	"github.com/TheMojtabam/ispof/internal/discover"
	"github.com/TheMojtabam/ispof/internal/events"
	"github.com/TheMojtabam/ispof/internal/procmgr"
	"github.com/TheMojtabam/ispof/internal/scraper"
	"github.com/TheMojtabam/ispof/internal/store"
	"github.com/TheMojtabam/ispof/internal/webui"
)

// Set at build time via -ldflags. Defaults are chosen so an unstripped
// `go run .` from a checkout still produces something sensible.
var (
	Version   = "v0.1.0"
	Commit    = "dev"
	BuildDate = "unknown"
)

func main() {
	var (
		listen         string
		tunnelsDir     string
		logLevel       string
		auth           string
		scrapeInterval time.Duration
		autoDiscover   bool
		showVersion    bool
	)

	flag.StringVar(&listen, "listen", "0.0.0.0:3000", "address:port for the panel to bind")
	flag.StringVar(&tunnelsDir, "tunnels-dir", "/etc/ispof/tunnels", "directory holding per-tunnel JSON configs")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug | info | warn | error")
	flag.StringVar(&auth, "auth", "", "basic auth credentials in user:password form (empty = no auth)")
	flag.DurationVar(&scrapeInterval, "scrape-interval", 10*time.Second, "Prometheus scrape interval")
	flag.BoolVar(&autoDiscover, "auto-discover", true, "scan filesystem at startup for unmanaged quiccochet configs")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("ispof %s (%s) built %s\n", Version, Commit, BuildDate)
		return
	}

	slog.SetDefault(newLogger(logLevel))

	st, err := store.New(tunnelsDir)
	if err != nil {
		fatal("init store", err)
	}
	pm, err := procmgr.New()
	if err != nil {
		slog.Warn("systemd not available, lifecycle ops will fail",
			slog.Any("err", err))
	}

	eventLog := events.New(500)
	scr := scraper.New(scraper.Config{
		Interval: scrapeInterval,
		Timeout:  3 * time.Second,
		Logf: func(format string, args ...any) {
			slog.Debug(fmt.Sprintf(format, args...))
		},
	})

	apiSrv := api.New(api.Deps{
		Store:   st,
		Proc:    pm,
		Scraper: scr,
		Events:  eventLog,
		Version: Version,
		Commit:  Commit,
		Built:   BuildDate,
	})

	handler, err := webui.New(webui.Config{
		APIServer: apiSrv,
		BasicAuth: auth,
	})
	if err != nil {
		fatal("init webui", err)
	}

	if auth == "" && !isLoopbackBind(listen) {
		slog.Warn("panel bound to non-loopback WITHOUT auth — anyone on the network can manage tunnels",
			slog.String("listen", listen))
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE streams stay open
		IdleTimeout:       5 * time.Minute,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Boot the background workers: Prometheus scraper, state-change
	// detector. Both follow ctx so they shut down with the rest of the
	// process.
	go scr.Start(ctx, apiSrv.ScraperTargets)
	go runStateDetector(ctx, apiSrv)

	// Startup discovery: scan the filesystem once for pre-existing
	// quiccochet configs (created by the upstream install script, by
	// hand, whatever) and emit an event so the UI can prompt to
	// import. Runs once at boot then sleeps forever.
	if autoDiscover {
		go runStartupDiscovery(ctx, st, eventLog)
	}

	go func() {
		slog.Info("ispof starting",
			slog.String("listen", listen),
			slog.String("version", Version),
			slog.String("tunnels_dir", tunnelsDir),
			slog.Duration("scrape_interval", scrapeInterval),
		)
		eventLog.Push(events.Event{
			Time:    time.Now(),
			Level:   events.Info,
			Type:    "panel_started",
			Message: fmt.Sprintf("ispof %s listening on %s", Version, listen),
		})
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", slog.Any("err", err))
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", slog.Any("err", err))
	}
}

// runStateDetector polls every 3 seconds for tunnel state transitions
// and pushes events into the log when transitions happen. We use a
// separate goroutine rather than reusing the scraper interval so state
// changes propagate faster than the metric refresh rate.
func runStateDetector(ctx context.Context, apiSrv *api.Server) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	// First check happens immediately so initial states are recorded
	// (this is what gives us a "previous" value to compare against).
	apiSrv.DetectStateChanges()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apiSrv.DetectStateChanges()
		}
	}
}

// runStartupDiscovery scans the filesystem once at boot for quiccochet
// configs the panel doesn't manage. For each, an event is emitted so
// the UI can show a banner like "found N existing tunnels — import?".
// Runs once and exits — no continuous CPU cost.
func runStartupDiscovery(ctx context.Context, st *store.Store, eventLog *events.Log) {
	// brief delay so the panel finishes wiring up first
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	scanCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	hits := discover.Discover(scanCtx, discover.Options{ExcludeDir: st.Dir()})
	if len(hits) == 0 {
		return
	}
	eventLog.Push(events.Event{
		Time:    time.Now(),
		Level:   events.Warn,
		Type:    "discovery",
		Message: fmt.Sprintf("found %d unmanaged quiccochet config(s) on the server — open Tunnels → Import", len(hits)),
	})
	slog.Info("startup discovery", slog.Int("hits", len(hits)))
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func isLoopbackBind(addr string) bool {
	return strings.HasPrefix(addr, "127.") ||
		strings.HasPrefix(addr, "localhost:") ||
		strings.HasPrefix(addr, "[::1]:")
}

func fatal(msg string, err error) {
	slog.Error(msg, slog.Any("err", err))
	os.Exit(1)
}

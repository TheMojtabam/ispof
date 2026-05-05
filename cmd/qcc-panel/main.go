package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pechenyeru/quiccochet/internal/panel/api"
	"github.com/pechenyeru/quiccochet/internal/panel/config"
)

const Version = "0.4.1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	switch cmd {
	case "panel":
		runPanel()
	case "version", "-v", "--version":
		fmt.Printf("qcc-panel %s\n", Version)
	case "genkey":
		runGenKey()
	case "pubkey":
		runPubKey()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `qcc-panel %s — QUICochet Web Panel

Usage:
  qcc-panel panel --config /etc/quiccochet/panel.json
  qcc-panel genkey                        # generate X25519 private key
  qcc-panel pubkey < private.key          # derive public key
  qcc-panel version

`, Version)
}

func runPanel() {
	fs := flag.NewFlagSet("panel", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/quiccochet/panel.json", "path to panel config")
	side := fs.String("side", "", "panel side (server|client). overrides config")
	_ = fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config load: %v", err)
	}
	if *side != "" {
		cfg.Side = *side
	}
	if cfg.Side == "" {
		cfg.Side = "server"
	}

	srv, err := api.NewServer(cfg)
	if err != nil {
		log.Fatalf("server init: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Printf("[qcc-panel] %s side listening on %s", cfg.Side, cfg.Listen)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("server: %v", err)
			cancel()
		}
	}()

	<-ctx.Done()
	log.Println("[qcc-panel] shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

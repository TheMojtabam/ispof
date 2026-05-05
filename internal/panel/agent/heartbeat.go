// Package agent runs the iran-side heartbeat ticker. When the panel is
// configured as side="client" with a non-empty foreign_panel URL and
// agent_token, this goroutine periodically POSTs a status snapshot to
// the foreign panel's /agent/heartbeat endpoint. The foreign panel
// stores the latest snapshot under its iran_status key, which feeds the
// "Iran" tab in the foreign UI.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pechenyeru/quiccochet/internal/panel/config"
	"github.com/pechenyeru/quiccochet/internal/panel/tunnel"
)

// HeartbeatPayload is the JSON shipped to the foreign panel each tick.
// It mirrors the IranStatus type the foreign panel expects.
type HeartbeatPayload struct {
	Connected bool      `json:"connected"`
	Version   string    `json:"version"`
	IranIP    string    `json:"iran_ip"`
	LastSeen  time.Time `json:"last_seen"`
	UptimeSec int64     `json:"uptime_sec"`
	CPU       float64   `json:"cpu_pct"`
	RAMMB     float64   `json:"ram_used_mb"`
	Forwards  int       `json:"forwards"`

	// Live tunnel stats forwarded straight from the daemon admin socket.
	UpMbps   float64 `json:"up_mbps"`
	DownMbps float64 `json:"down_mbps"`
	Sessions int     `json:"sessions"`
	LossPct  float64 `json:"loss_pct"`
	RTTMs    float64 `json:"rtt_ms"`
}

// Heartbeater pushes status to the foreign panel on a fixed interval.
type Heartbeater struct {
	cfg    *config.Config
	tm     *tunnel.Manager
	cli    *http.Client
	stop   chan struct{}
	logger *slog.Logger
}

// New creates a heartbeater. It is safe to call Start even if the panel
// isn't configured as a client — the ticker will silently exit.
func New(cfg *config.Config, tm *tunnel.Manager) *Heartbeater {
	return &Heartbeater{
		cfg:    cfg,
		tm:     tm,
		cli:    &http.Client{Timeout: 5 * time.Second},
		stop:   make(chan struct{}),
		logger: slog.Default(),
	}
}

// Start launches the ticker in a goroutine. Returns immediately.
func (h *Heartbeater) Start() {
	if h.cfg.Side != "client" {
		return
	}
	if strings.TrimSpace(h.cfg.ForeignPanel) == "" {
		h.logger.Info("heartbeat disabled: no foreign_panel set")
		return
	}
	if strings.TrimSpace(h.cfg.AgentToken) == "" {
		h.logger.Warn("heartbeat disabled: no agent_token set")
		return
	}
	go h.loop()
}

func (h *Heartbeater) Stop() {
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
}

func (h *Heartbeater) loop() {
	// First beat after 3s so the panel finishes startup; then every 15s.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-h.stop:
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			if err := h.beat(ctx); err != nil {
				h.logger.Warn("heartbeat failed", "err", err)
			}
			cancel()
			timer.Reset(15 * time.Second)
		}
	}
}

func (h *Heartbeater) beat(ctx context.Context) error {
	snap := h.tm.Snapshot()

	hb := HeartbeatPayload{
		Connected: snap.Source == "live",
		Version:   "0.4.1",
		IranIP:    detectPublicIP(),
		LastSeen:  time.Now(),
		UptimeSec: int64(snap.UptimeSec),
		UpMbps:    snap.UpMbps,
		DownMbps:  snap.DownMbps,
		Sessions:  snap.Sessions,
		LossPct:   snap.LossPct,
		RTTMs:     snap.RTTMs,
	}

	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	url := strings.TrimRight(h.cfg.ForeignPanel, "/") + "/agent/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", h.cfg.AgentToken)

	resp, err := h.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errors.New("foreign rejected heartbeat: " + resp.Status)
	}
	return nil
}

// detectPublicIP tries the local hostname's first non-loopback address. We
// avoid hitting an external service from the heartbeat path — the foreign
// panel only needs a stable identifier, not a perfect public address.
func detectPublicIP() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		if addrs, err := net.LookupHost(h); err == nil {
			for _, a := range addrs {
				if !strings.HasPrefix(a, "127.") && !strings.HasPrefix(a, "::1") {
					return a
				}
			}
		}
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifs {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return ""
}

package api

import (
	"encoding/json"
	"net/http"
	"os/exec"

	"github.com/pechenyeru/quiccochet/internal/panel/tunnel"
)

func (s *Server) handleTunnelGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.tunnel.Get()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, cfg)
}

func (s *Server) handleTunnelPut(w http.ResponseWriter, r *http.Request) {
	var cfg tunnel.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.tunnel.Save(&cfg); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	go s.notify.Send("tunnel-config", "Tunnel config updated")
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

func (s *Server) handleTunnelRestart(w http.ResponseWriter, _ *http.Request) {
	svc := "quiccochet-server"
	if s.cfg.Side == "client" {
		svc = "quiccochet-client"
	}
	if out, err := exec.Command("systemctl", "restart", svc).CombinedOutput(); err != nil {
		writeErr(w, 500, string(out))
		return
	}
	writeJSON(w, 200, map[string]string{"status": "restarted"})
}

func (s *Server) handleTunnelStop(w http.ResponseWriter, _ *http.Request) {
	svc := "quiccochet-server"
	if s.cfg.Side == "client" {
		svc = "quiccochet-client"
	}
	_, _ = exec.Command("systemctl", "stop", svc).CombinedOutput()
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, _ *http.Request) {
	svc := "quiccochet-server"
	if s.cfg.Side == "client" {
		svc = "quiccochet-client"
	}
	_, _ = exec.Command("systemctl", "start", svc).CombinedOutput()
	writeJSON(w, 200, map[string]string{"status": "started"})
}

func (s *Server) handleGenKey(w http.ResponseWriter, _ *http.Request) {
	priv, pub, err := tunnel.GenerateKeyPair()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"private": priv, "public": pub})
}

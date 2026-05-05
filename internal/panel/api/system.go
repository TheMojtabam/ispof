package api

import (
	"bufio"
	"net/http"
	"os/exec"
	"strings"

	"github.com/go-chi/chi/v5"
)

// SysInfo combines kernel/cpu/memory snapshot for the dashboard system widget.
type SysInfoResponse struct {
	Kernel    string   `json:"kernel"`
	OS        string   `json:"os"`
	CPUPct    float64  `json:"cpu_pct"`
	RAMUsedMB float64  `json:"ram_used_mb"`
	RAMTotMB  float64  `json:"ram_total_mb"`
	DiskUsed  float64  `json:"disk_used_pct"`
	NetIfaces []string `json:"net_ifaces"`
	UptimeSec int64    `json:"uptime_sec"`
}

func (s *Server) handleSysInfo(w http.ResponseWriter, _ *http.Request) {
	cpu, ramU, ramT := s.sysctl.ResourceSnapshot()
	info := SysInfoResponse{
		Kernel:    s.sysctl.Kernel(),
		OS:        s.sysctl.OSRelease(),
		CPUPct:    cpu,
		RAMUsedMB: ramU,
		RAMTotMB:  ramT,
		DiskUsed:  s.sysctl.DiskUsedPct("/"),
		NetIfaces: s.sysctl.NetIfaces(),
		UptimeSec: int64(s.sysctl.UptimeSec()),
	}
	writeJSON(w, 200, info)
}

func (s *Server) handleSysctlGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.sysctl.GetTuning())
}

func (s *Server) handleSysctlApply(w http.ResponseWriter, _ *http.Request) {
	if err := s.sysctl.ApplyTuning(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "applied"})
}

func (s *Server) handleFirewallGet(w http.ResponseWriter, _ *http.Request) {
	rules, err := s.sysctl.FirewallStatus()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rules)
}

func (s *Server) handleFirewallAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule string `json:"rule"`
	}
	_ = decodeJSON(r, &req)
	if err := s.sysctl.FirewallAdd(req.Rule); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "added"})
}

// handleServiceCtl: POST /service/{name}/{action} where action ∈ start|stop|restart|enable|disable
func (s *Server) handleServiceCtl(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	action := chi.URLParam(r, "action")
	allowedActions := map[string]bool{"start": true, "stop": true, "restart": true, "enable": true, "disable": true, "status": true}
	allowedSvcs := map[string]bool{"quiccochet-server": true, "quiccochet-client": true, "quiccochet-panel": true, "xray": true}
	if !allowedActions[action] || !allowedSvcs[name] {
		writeErr(w, 400, "service or action not allowed")
		return
	}
	out, err := exec.Command("systemctl", action, name).CombinedOutput()
	if err != nil && action != "status" {
		writeErr(w, 500, string(out))
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "out": string(out)})
}

// handleLogTail returns the last 200 lines from a journal/file by service name.
func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if svc == "" {
		svc = "quiccochet-server"
		if s.cfg.Side == "client" {
			svc = "quiccochet-client"
		}
	}
	out, _ := exec.Command("journalctl", "-u", svc, "-n", "200", "--no-pager", "-o", "short-iso").CombinedOutput()
	lines := []string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	writeJSON(w, 200, map[string]any{"service": svc, "lines": lines})
}

// handleMetricsTS returns recent timeseries for the dashboard charts.
func (s *Server) handleMetricsTS(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "60s"
	}
	writeJSON(w, 200, s.tunnel.Timeseries(rng))
}

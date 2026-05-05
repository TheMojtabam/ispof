package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
)

// ---- Dashboard ----

type DashSnap struct {
	Side       string  `json:"side"`
	UpMbps     float64 `json:"up_mbps"`
	DownMbps   float64 `json:"down_mbps"`
	Sessions   int     `json:"sessions"`
	LossPct    float64 `json:"loss_pct"`
	RTTMs      float64 `json:"rtt_ms"`
	CPU        float64 `json:"cpu_pct"`
	RAMUsedMB  float64 `json:"ram_used_mb"`
	RAMTotalMB float64 `json:"ram_total_mb"`
	UptimeSec  int64   `json:"uptime_sec"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	snap := s.tunnel.Snapshot()
	cpu, ramU, ramT := s.sysctl.ResourceSnapshot()
	out := DashSnap{
		Side:       s.cfg.Side,
		UpMbps:     snap.UpMbps,
		DownMbps:   snap.DownMbps,
		Sessions:   snap.Sessions,
		LossPct:    snap.LossPct,
		RTTMs:      snap.RTTMs,
		CPU:        cpu,
		RAMUsedMB:  ramU,
		RAMTotalMB: ramT,
		UptimeSec:  int64(time.Since(startedAt).Seconds()),
	}
	writeJSON(w, 200, out)
	runtime.Gosched()
}

var startedAt = time.Now()

// ---- Routing rules ----

type Rule struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // domain, ip, protocol, port
	Patterns []string `json:"patterns"`
	Target   string   `json:"target"` // direct, block, proxy, tag:<name>
	Priority int      `json:"priority"`
}

func (s *Server) handleRoutingList(w http.ResponseWriter, _ *http.Request) {
	out := []*Rule{}
	_ = s.store.List(store.BucketRouting, func(_ string, raw []byte) error {
		r := &Rule{}
		if err := json.Unmarshal(raw, r); err == nil {
			out = append(out, r)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	writeJSON(w, 200, out)
}

func (s *Server) handleRoutingCreate(w http.ResponseWriter, r *http.Request) {
	rule := &Rule{}
	if err := json.NewDecoder(r.Body).Decode(rule); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if rule.ID == "" {
		rule.ID = randID(6)
	}
	if err := s.store.Put(store.BucketRouting, rule.ID, rule); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.regenXrayConfig()
	writeJSON(w, 201, rule)
}

func (s *Server) handleRoutingDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.store.Delete(store.BucketRouting, id)
	_ = s.regenXrayConfig()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleGeoUpdate(w http.ResponseWriter, _ *http.Request) {
	// TODO: download geoip.dat / geosite.dat from Loyalsoldier release
	writeJSON(w, 200, map[string]any{"status": "todo", "message": "geo update will fetch from Loyalsoldier release"})
}

// ---- Forwards (iran side) ----

type Forward struct {
	ID         string    `json:"id"`
	IranPort   int       `json:"iran_port"`
	Protocol   string    `json:"protocol"` // tcp, udp
	TargetTag  string    `json:"target_tag"`
	TargetPort int       `json:"target_port"`
	Enabled    bool      `json:"enabled"`
	BytesToday int64     `json:"bytes_today"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Server) handleForwardsList(w http.ResponseWriter, _ *http.Request) {
	out := []*Forward{}
	_ = s.store.List(store.BucketForwards, func(_ string, raw []byte) error {
		f := &Forward{}
		if err := json.Unmarshal(raw, f); err == nil {
			out = append(out, f)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].IranPort < out[j].IranPort })
	writeJSON(w, 200, out)
}

func (s *Server) handleForwardCreate(w http.ResponseWriter, r *http.Request) {
	f := &Forward{}
	if err := json.NewDecoder(r.Body).Decode(f); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if f.ID == "" {
		f.ID = randID(6)
	}
	f.Enabled = true
	f.CreatedAt = time.Now()
	if err := s.store.Put(store.BucketForwards, f.ID, f); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// TODO: actually program iptables/nftables here
	writeJSON(w, 201, f)
}

func (s *Server) handleForwardDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.store.Delete(store.BucketForwards, id)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

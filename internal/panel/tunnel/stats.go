package tunnel

import (
	"encoding/json"
	"time"
)

// Snapshot is the live performance snapshot of the tunnel as observed from
// outside the daemon (i.e. by the panel querying the admin Unix socket).
//
// Up/Down rates are derived by the panel from successive byte counter
// deltas; the daemon itself only emits cumulative counters.
type Snapshot struct {
	UpMbps   float64 `json:"up_mbps"`
	DownMbps float64 `json:"down_mbps"`
	Sessions int     `json:"sessions"`
	LossPct  float64 `json:"loss_pct"`
	RTTMs    float64 `json:"rtt_ms"`

	BytesSent     uint64  `json:"bytes_sent"`
	BytesReceived uint64  `json:"bytes_received"`
	UptimeSec     float64 `json:"uptime_sec"`
	Source        string  `json:"source"` // "live" or "stub"
}

// adminSnapshot mirrors the JSON shape emitted by the daemon's admin socket
// (internal/admin/admin.go in this repo). Fields we don't need are ignored.
type adminSnapshot struct {
	Role           string  `json:"role"`
	PoolAlive      int     `json:"pool_alive"`
	PoolTotal      int     `json:"pool_total"`
	UDPAssocs      int     `json:"udp_assocs"`
	ActiveSessions int32   `json:"active_sessions"`
	BytesSent      uint64  `json:"bytes_sent"`
	BytesReceived  uint64  `json:"bytes_received"`
	PacketsSent    uint64  `json:"packets_sent"`
	PacketsLost    uint64  `json:"packets_lost"`
	UptimeSec      float64 `json:"uptime_sec"`
}

// Snapshot fetches a live snapshot from the daemon's admin socket.
// On error (daemon not running, socket missing) we return the last good
// snapshot or zeros.
func (m *Manager) Snapshot() Snapshot {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()

	raw, err := m.queryAdmin("stats")
	if err != nil {
		// daemon not reachable — return last known
		m.lastSnap.Source = "stale"
		return m.lastSnap
	}
	a := adminSnapshot{}
	if err := json.Unmarshal(raw, &a); err != nil {
		m.lastSnap.Source = "stale"
		return m.lastSnap
	}

	now := time.Now()
	out := Snapshot{
		Sessions:      int(a.ActiveSessions) + a.UDPAssocs + a.PoolAlive,
		BytesSent:     a.BytesSent,
		BytesReceived: a.BytesReceived,
		UptimeSec:     a.UptimeSec,
		Source:        "live",
	}
	if a.PacketsSent > 0 {
		out.LossPct = float64(a.PacketsLost) * 100.0 / float64(a.PacketsSent)
	}
	if !m.prevAt.IsZero() {
		dt := now.Sub(m.prevAt).Seconds()
		if dt > 0.1 {
			if a.BytesSent >= m.prevTx {
				out.UpMbps = float64(a.BytesSent-m.prevTx) * 8 / dt / 1e6
			}
			if a.BytesReceived >= m.prevRx {
				out.DownMbps = float64(a.BytesReceived-m.prevRx) * 8 / dt / 1e6
			}
		}
	}
	m.prevAt = now
	m.prevTx = a.BytesSent
	m.prevRx = a.BytesReceived
	m.lastSnap = out

	// keep a small ring of history for the chart
	m.history = append(m.history, SamplePoint{At: now, UpMbps: out.UpMbps, DownMbps: out.DownMbps})
	if len(m.history) > 120 {
		m.history = m.history[len(m.history)-120:]
	}
	return out
}

// Timeseries returns up to ~2min of (timestamp, up, down) samples.
func (m *Manager) Timeseries(window string) [][3]float64 {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	out := make([][3]float64, 0, len(m.history))
	for _, p := range m.history {
		out = append(out, [3]float64{float64(p.At.Unix()), p.UpMbps, p.DownMbps})
	}
	return out
}

// SamplePoint is one entry in the rolling history.
type SamplePoint struct {
	At       time.Time
	UpMbps   float64
	DownMbps float64
}

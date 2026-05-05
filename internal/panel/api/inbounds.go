package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
	"github.com/pechenyeru/quiccochet/internal/panel/xray"
)

// Inbound represents an Xray inbound on the foreign side, paired with its iran-side port.
type Inbound struct {
	ID              string    `json:"id"`
	Tag             string    `json:"tag"`
	Protocol        string    `json:"protocol"`   // vless, vmess, trojan, shadowsocks
	Network         string    `json:"network"`    // tcp, ws, grpc
	LocalPort       int       `json:"local_port"` // 8001-8099 on foreign
	IranPort        int       `json:"iran_port"`  // public port on iran (e.g. 443)
	Security        string    `json:"security"`   // none, tls, reality
	SNI             string    `json:"sni,omitempty"`
	Flow            string    `json:"flow,omitempty"`
	WSPath          string    `json:"ws_path,omitempty"`
	GRPCSvcName     string    `json:"grpc_svc_name,omitempty"`
	RealityDest     string    `json:"reality_dest,omitempty"`
	RealityPubkey   string    `json:"reality_pubkey,omitempty"`
	RealityShortIDs []string  `json:"reality_short_ids,omitempty"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

func (s *Server) handleInboundsList(w http.ResponseWriter, _ *http.Request) {
	out := []*Inbound{}
	_ = s.store.List(store.BucketInbounds, func(_ string, raw []byte) error {
		ib := &Inbound{}
		if err := json.Unmarshal(raw, ib); err == nil {
			out = append(out, ib)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	writeJSON(w, 200, out)
}

func (s *Server) handleInboundCreate(w http.ResponseWriter, r *http.Request) {
	ib := &Inbound{}
	if err := json.NewDecoder(r.Body).Decode(ib); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ib.Tag == "" {
		writeErr(w, 400, "tag required")
		return
	}
	if ib.ID == "" {
		ib.ID = randID(6)
	}
	ib.Enabled = true
	ib.CreatedAt = time.Now()
	if err := s.store.Put(store.BucketInbounds, ib.ID, ib); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.regenXrayConfig(); err != nil {
		writeErr(w, 500, "xray regen: "+err.Error())
		return
	}
	writeJSON(w, 201, ib)
}

func (s *Server) handleInboundGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ib := &Inbound{}
	if err := s.store.Get(store.BucketInbounds, id, ib); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, ib)
}

func (s *Server) handleInboundUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cur := &Inbound{}
	if err := s.store.Get(store.BucketInbounds, id, cur); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	patch := &Inbound{}
	if err := json.NewDecoder(r.Body).Decode(patch); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	patch.ID = cur.ID
	patch.CreatedAt = cur.CreatedAt
	if err := s.store.Put(store.BucketInbounds, id, patch); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.regenXrayConfig()
	writeJSON(w, 200, patch)
}

func (s *Server) handleInboundDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.Delete(store.BucketInbounds, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.regenXrayConfig()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// regenXrayConfig builds /etc/xray/config.json from current inbounds + users
// and asks the xray service to reload. NOTE: this assumes the user has
// xray-core installed and a systemd unit named "xray".
func (s *Server) regenXrayConfig() error {
	if s.cfg.XrayConfig == "" {
		return nil
	}
	inbounds := []*Inbound{}
	_ = s.store.List(store.BucketInbounds, func(_ string, raw []byte) error {
		ib := &Inbound{}
		if json.Unmarshal(raw, ib) == nil && ib.Enabled {
			inbounds = append(inbounds, ib)
		}
		return nil
	})

	users := []*User{}
	_ = s.store.List(store.BucketUsers, func(_ string, raw []byte) error {
		u := &User{}
		if json.Unmarshal(raw, u) == nil && u.Enabled {
			users = append(users, u)
		}
		return nil
	})

	xrayInbounds := make([]xray.Inbound, 0, len(inbounds))
	for _, ib := range inbounds {
		xrayInbounds = append(xrayInbounds, xray.Inbound{
			Tag:             ib.Tag,
			Protocol:        ib.Protocol,
			Network:         ib.Network,
			LocalPort:       ib.LocalPort,
			Security:        ib.Security,
			SNI:             ib.SNI,
			Flow:            ib.Flow,
			WSPath:          ib.WSPath,
			GRPCSvcName:     ib.GRPCSvcName,
			RealityDest:     ib.RealityDest,
			RealityShortIDs: ib.RealityShortIDs,
		})
	}
	xrayUsers := make([]xray.User, 0, len(users))
	for _, u := range users {
		xrayUsers = append(xrayUsers, xray.User{
			Name:       u.Name,
			UUID:       u.UUID,
			InboundTag: u.InboundTag,
			Flow:       "",
		})
	}
	return xray.WriteConfig(s.cfg.XrayConfig, xrayInbounds, xrayUsers)
}

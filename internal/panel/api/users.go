package api

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
)

// User represents a panel-managed end user.
type User struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	UUID       string    `json:"uuid"`
	InboundTag string    `json:"inbound_tag"`
	QuotaBytes int64     `json:"quota_bytes"`
	UsedBytes  int64     `json:"used_bytes"`
	ExpiresAt  time.Time `json:"expires_at"`
	Group      string    `json:"group"`
	Enabled    bool      `json:"enabled"`
	Notes      string    `json:"notes,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen,omitempty"`
}

func (s *Server) handleUsersList(w http.ResponseWriter, _ *http.Request) {
	out := []*User{}
	_ = s.store.List(store.BucketUsers, func(_ string, raw []byte) error {
		u := &User{}
		if err := json.Unmarshal(raw, u); err == nil {
			out = append(out, u)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, out)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	u := &User{}
	if err := json.NewDecoder(r.Body).Decode(u); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if u.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	if u.UUID == "" {
		u.UUID = newUUID()
	}
	if u.ID == "" {
		u.ID = randID(8)
	}
	if u.Group == "" {
		u.Group = "basic"
	}
	u.Enabled = true
	u.CreatedAt = time.Now()

	if err := s.store.Put(store.BucketUsers, u.ID, u); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	go s.notify.Send("user-create", "new user: "+u.Name)
	writeJSON(w, 201, u)
}

func (s *Server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u := &User{}
	if err := s.store.Get(store.BucketUsers, id, u); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cur := &User{}
	if err := s.store.Get(store.BucketUsers, id, cur); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	patch := &User{}
	if err := json.NewDecoder(r.Body).Decode(patch); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	patch.ID = cur.ID
	patch.UUID = cur.UUID // immutable unless reset endpoint
	patch.CreatedAt = cur.CreatedAt
	if err := s.store.Put(store.BucketUsers, id, patch); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, patch)
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.Delete(store.BucketUsers, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleUserResetUUID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u := &User{}
	if err := s.store.Get(store.BucketUsers, id, u); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	u.UUID = newUUID()
	_ = s.store.Put(store.BucketUsers, id, u)
	writeJSON(w, 200, u)
}

func (s *Server) handleUsersCSV(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="users.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "name", "uuid", "inbound", "quota_bytes", "used_bytes", "group", "expires_at", "enabled"})
	_ = s.store.List(store.BucketUsers, func(_ string, raw []byte) error {
		u := &User{}
		if json.Unmarshal(raw, u) != nil {
			return nil
		}
		_ = cw.Write([]string{
			u.ID, u.Name, u.UUID, u.InboundTag,
			fmt.Sprintf("%d", u.QuotaBytes), fmt.Sprintf("%d", u.UsedBytes),
			u.Group, u.ExpiresAt.Format("2006-01-02"),
			fmt.Sprintf("%v", u.Enabled),
		})
		return nil
	})
	cw.Flush()
}

// ---- helpers ----

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

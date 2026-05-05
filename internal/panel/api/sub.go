package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/pechenyeru/quiccochet/internal/panel/store"
	qrcode "github.com/skip2/go-qrcode"
)

// handleSubscription returns a newline-separated list of vmess/vless/trojan URIs
// for every enabled inbound the user is allowed to use.
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	userName := chi.URLParam(r, "user")
	user := s.findUserByName(userName)
	if user == nil {
		writeErr(w, 404, "user not found")
		return
	}
	inbounds := []*Inbound{}
	_ = s.store.List(store.BucketInbounds, func(_ string, raw []byte) error {
		ib := &Inbound{}
		if json.Unmarshal(raw, ib) == nil && ib.Enabled {
			if user.InboundTag == "" || user.InboundTag == ib.Tag {
				inbounds = append(inbounds, ib)
			}
		}
		return nil
	})

	host := s.cfg.Domain
	if host == "" {
		host = strings.Split(r.Host, ":")[0]
	}

	var buf bytes.Buffer
	for _, ib := range inbounds {
		uri := buildURI(host, user, ib)
		if uri != "" {
			buf.WriteString(uri)
			buf.WriteByte('\n')
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if user.QuotaBytes > 0 {
		w.Header().Set("Subscription-Userinfo",
			fmt.Sprintf("upload=0; download=%d; total=%d; expire=%d",
				user.UsedBytes, user.QuotaBytes, user.ExpiresAt.Unix()))
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	userName := chi.URLParam(r, "user")
	user := s.findUserByName(userName)
	if user == nil {
		writeErr(w, 404, "user not found")
		return
	}
	host := s.cfg.Domain
	if host == "" {
		host = strings.Split(r.Host, ":")[0]
	}
	// pick first enabled inbound for this user
	var pick *Inbound
	_ = s.store.List(store.BucketInbounds, func(_ string, raw []byte) error {
		ib := &Inbound{}
		if json.Unmarshal(raw, ib) == nil && ib.Enabled {
			if pick == nil && (user.InboundTag == "" || user.InboundTag == ib.Tag) {
				pick = ib
			}
		}
		return nil
	})
	if pick == nil {
		writeErr(w, 404, "no inbound for user")
		return
	}
	uri := buildURI(host, user, pick)
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (s *Server) findUserByName(name string) *User {
	var found *User
	_ = s.store.List(store.BucketUsers, func(_ string, raw []byte) error {
		u := &User{}
		if json.Unmarshal(raw, u) == nil && u.Name == name {
			found = u
		}
		return nil
	})
	return found
}

// buildURI constructs the appropriate proxy URI for a (user, inbound) pair.
func buildURI(host string, u *User, ib *Inbound) string {
	port := ib.IranPort
	if port == 0 {
		port = ib.LocalPort
	}
	switch ib.Protocol {
	case "vless":
		q := url.Values{}
		q.Set("type", ifBlank(ib.Network, "tcp"))
		q.Set("security", ifBlank(ib.Security, "none"))
		if ib.SNI != "" {
			q.Set("sni", ib.SNI)
		}
		if ib.Flow != "" {
			q.Set("flow", ib.Flow)
		}
		if ib.WSPath != "" {
			q.Set("path", ib.WSPath)
		}
		if ib.GRPCSvcName != "" {
			q.Set("serviceName", ib.GRPCSvcName)
		}
		if ib.Security == "reality" {
			if ib.RealityPubkey != "" {
				q.Set("pbk", ib.RealityPubkey)
			}
			if len(ib.RealityShortIDs) > 0 {
				q.Set("sid", ib.RealityShortIDs[0])
			}
			q.Set("fp", "chrome")
		}
		return fmt.Sprintf("vless://%s@%s:%d?%s#%s",
			u.UUID, host, port, q.Encode(), url.QueryEscape(u.Name+"@"+ib.Tag))
	case "trojan":
		q := url.Values{}
		q.Set("security", "tls")
		if ib.SNI != "" {
			q.Set("sni", ib.SNI)
		}
		return fmt.Sprintf("trojan://%s@%s:%d?%s#%s",
			u.UUID, host, port, q.Encode(), url.QueryEscape(u.Name+"@"+ib.Tag))
	case "shadowsocks":
		// SS-2022 form: ss://method:password@host:port#name
		return fmt.Sprintf("ss://2022-blake3-aes-128-gcm:%s@%s:%d#%s",
			u.UUID, host, port, url.QueryEscape(u.Name+"@"+ib.Tag))
	default:
		return ""
	}
}

func ifBlank(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

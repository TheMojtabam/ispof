// Package xray builds an Xray-core configuration from panel-managed inbounds.
//
// The output is a minimal but production-shaped Xray config JSON. The user is
// expected to run xray-core (https://github.com/XTLS/Xray-core) under systemd
// pointing at the file we write here.
package xray

import (
	"encoding/json"
	"os"
)

// Inbound is the panel-side notion of an Xray inbound.
type Inbound struct {
	Tag             string
	Protocol        string
	Network         string
	LocalPort       int
	Security        string
	SNI             string
	Flow            string
	WSPath          string
	GRPCSvcName     string
	RealityDest     string
	RealityShortIDs []string
}

// User is a client allowed to connect to inbounds.
type User struct {
	Name       string
	UUID       string
	InboundTag string
	Flow       string
}

// WriteConfig writes a complete Xray config.json to path.
func WriteConfig(path string, inbounds []Inbound, users []User) error {
	cfg := map[string]any{
		"log": map[string]any{
			"loglevel": "warning",
			"access":   "/var/log/xray/access.log",
			"error":    "/var/log/xray/error.log",
		},
		"dns": map[string]any{
			"servers": []any{"1.1.1.1", "8.8.8.8", "https://dns.cloudflare.com/dns-query"},
		},
		"inbounds":  buildInbounds(inbounds, users),
		"outbounds": defaultOutbounds(),
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{"type": "field", "ip": []string{"geoip:private"}, "outboundTag": "block"},
				map[string]any{"type": "field", "domain": []string{"geosite:category-ads-all"}, "outboundTag": "block"},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func buildInbounds(inbounds []Inbound, users []User) []any {
	out := make([]any, 0, len(inbounds))
	for _, ib := range inbounds {
		clients := []any{}
		for _, u := range users {
			if u.InboundTag == ib.Tag {
				c := map[string]any{"id": u.UUID, "email": u.Name}
				if ib.Flow != "" {
					c["flow"] = ib.Flow
				}
				clients = append(clients, c)
			}
		}
		stream := streamSettings(ib)
		settings := map[string]any{}
		switch ib.Protocol {
		case "vless":
			settings["clients"] = clients
			settings["decryption"] = "none"
		case "vmess":
			settings["clients"] = clients
		case "trojan":
			pwd := []any{}
			for _, u := range users {
				if u.InboundTag == ib.Tag {
					pwd = append(pwd, map[string]any{"password": u.UUID, "email": u.Name})
				}
			}
			settings["clients"] = pwd
		case "shadowsocks":
			settings["method"] = "2022-blake3-aes-128-gcm"
			settings["network"] = "tcp,udp"
			pwd := []any{}
			for _, u := range users {
				if u.InboundTag == ib.Tag {
					pwd = append(pwd, map[string]any{"password": u.UUID, "email": u.Name})
				}
			}
			settings["clients"] = pwd
		}
		out = append(out, map[string]any{
			"tag":            ib.Tag,
			"listen":         "127.0.0.1",
			"port":           ib.LocalPort,
			"protocol":       ib.Protocol,
			"settings":       settings,
			"streamSettings": stream,
			"sniffing": map[string]any{
				"enabled":      true,
				"destOverride": []string{"http", "tls", "quic"},
			},
		})
	}
	return out
}

func streamSettings(ib Inbound) map[string]any {
	ss := map[string]any{
		"network":  ifBlank(ib.Network, "tcp"),
		"security": ifBlank(ib.Security, "none"),
	}
	switch ib.Network {
	case "ws":
		ss["wsSettings"] = map[string]any{"path": ifBlank(ib.WSPath, "/")}
	case "grpc":
		ss["grpcSettings"] = map[string]any{"serviceName": ifBlank(ib.GRPCSvcName, "qcc")}
	}
	switch ib.Security {
	case "tls":
		ss["tlsSettings"] = map[string]any{"serverName": ib.SNI, "alpn": []string{"h2", "http/1.1"}}
	case "reality":
		shortIDs := ib.RealityShortIDs
		if len(shortIDs) == 0 {
			shortIDs = []string{""}
		}
		ss["realitySettings"] = map[string]any{
			"show":        false,
			"dest":        ifBlank(ib.RealityDest, "cdn.akamai.net:443"),
			"serverNames": []string{ifBlank(ib.SNI, "cdn.akamai.net")},
			"shortIds":    shortIDs,
		}
	}
	return ss
}

func defaultOutbounds() []any {
	return []any{
		map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"domainStrategy": "UseIPv4"}},
		map[string]any{"tag": "block", "protocol": "blackhole", "settings": map[string]any{}},
		map[string]any{"tag": "dns-out", "protocol": "dns"},
	}
}

func ifBlank(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

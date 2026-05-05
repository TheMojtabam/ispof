// Package notify dispatches event notifications to configured channels.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/pechenyeru/quiccochet/internal/panel/config"
)

// Dispatcher fans events out to all enabled channels.
type Dispatcher struct {
	cfg *config.Config
	cli *http.Client
}

func NewDispatcher(cfg *config.Config) *Dispatcher {
	return &Dispatcher{cfg: cfg, cli: &http.Client{Timeout: 8 * time.Second}}
}

// Send delivers a short event message. Best-effort, errors are logged.
func (d *Dispatcher) Send(event, msg string) {
	if d.cfg.Telegram.Enabled && d.cfg.Telegram.BotToken != "" {
		if err := d.sendTelegram(event, msg); err != nil {
			log.Printf("notify telegram: %v", err)
		}
	}
	if d.cfg.Discord.Enabled && d.cfg.Discord.WebhookURL != "" {
		if err := d.sendDiscord(event, msg); err != nil {
			log.Printf("notify discord: %v", err)
		}
	}
}

func (d *Dispatcher) sendTelegram(event, msg string) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.cfg.Telegram.BotToken)
	body := url.Values{}
	body.Set("chat_id", d.cfg.Telegram.ChatID)
	body.Set("text", fmt.Sprintf("[%s] %s", event, msg))
	body.Set("parse_mode", "HTML")
	resp, err := d.cli.PostForm(api, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram: %s", resp.Status)
	}
	return nil
}

func (d *Dispatcher) sendDiscord(event, msg string) error {
	payload := map[string]any{
		"username": "QUICochet",
		"content":  fmt.Sprintf("**%s** — %s", event, msg),
	}
	b, _ := json.Marshal(payload)
	resp, err := d.cli.Post(d.cfg.Discord.WebhookURL, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord: %s", resp.Status)
	}
	return nil
}

// Test fires a "hello" event on every enabled channel; returns the per-channel error.
func (d *Dispatcher) Test() map[string]string {
	out := map[string]string{}
	if d.cfg.Telegram.Enabled {
		if err := d.sendTelegram("test", "hello from QUICochet panel"); err != nil {
			out["telegram"] = err.Error()
		} else {
			out["telegram"] = "ok"
		}
	}
	if d.cfg.Discord.Enabled {
		if err := d.sendDiscord("test", "hello from QUICochet panel"); err != nil {
			out["discord"] = err.Error()
		} else {
			out["discord"] = "ok"
		}
	}
	return out
}

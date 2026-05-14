// Package events is an in-memory ring buffer of lifecycle events.
// Events are pushed from places that know they happened (API handlers
// after a successful start/stop, the scraper when state transitions are
// observed) and consumed by the UI through SSE and the /api/events
// endpoint.
//
// Why in-memory only? Lifecycle events are interesting in the moment but
// boring after restart — the persistent record of what tunnels exist
// lives in /etc/ispof/tunnels and the persistent record of what they
// did lives in journalctl. Duplicating either would be a maintenance
// burden for no clear win.
package events

import (
	"sync"
	"time"
)

// Level taxonomy mirrors what most log UIs already understand. We pick
// a small fixed set so the CSS for badges doesn't have to be open-ended.
type Level string

const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Event is one entry in the log. JSON tags are lowercase to match the
// rest of the API.
type Event struct {
	Time    time.Time `json:"time"`
	Level   Level     `json:"level"`
	Type    string    `json:"type"`    // "started" | "stopped" | "restarted" | "created" | "updated" | "deleted" | "state_change" | "scrape_error"
	Tunnel  string    `json:"tunnel,omitempty"`
	Message string    `json:"message"`
}

// Log is the ring buffer. Construct with New().
type Log struct {
	mu       sync.RWMutex
	buf      []Event
	head     int
	size     int
	listeners []chan Event // for live streaming
}

func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 200
	}
	return &Log{buf: make([]Event, capacity), size: capacity}
}

// Push records a new event. It is safe to call from any goroutine.
// Listeners attached via Subscribe receive the event before Push returns
// — listeners must not block (use buffered channels).
func (l *Log) Push(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Level == "" {
		e.Level = Info
	}
	l.mu.Lock()
	l.buf[l.head] = e
	l.head = (l.head + 1) % l.size
	listeners := make([]chan Event, len(l.listeners))
	copy(listeners, l.listeners)
	l.mu.Unlock()

	for _, ch := range listeners {
		select {
		case ch <- e:
		default:
			// drop — slow listener, not our problem to block other listeners
		}
	}
}

// Recent returns the most recent N events in reverse chronological order
// (newest first). Pass n <= 0 to get everything in the buffer.
func (l *Log) Recent(n int) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > l.size {
		n = l.size
	}
	out := make([]Event, 0, n)
	for i := 0; i < l.size && len(out) < n; i++ {
		idx := (l.head - 1 - i + l.size) % l.size
		e := l.buf[idx]
		if !e.Time.IsZero() {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel that receives every event pushed after
// Subscribe was called. Capacity sets the channel's buffer; values
// dropped due to full buffer are silently lost (this is a live stream,
// not a guaranteed delivery channel).
//
// Callers MUST call Unsubscribe when done to release the channel and
// avoid leaking listeners.
func (l *Log) Subscribe(capacity int) chan Event {
	if capacity <= 0 {
		capacity = 16
	}
	ch := make(chan Event, capacity)
	l.mu.Lock()
	l.listeners = append(l.listeners, ch)
	l.mu.Unlock()
	return ch
}

// Unsubscribe removes a channel from the listener list. The channel is
// NOT closed because Push runs without holding the lock while it sends
// to its copy of the listener slice — closing here would race with a
// concurrent Push that copied the slice before our removal landed, and
// `send on closed channel` panics regardless of select+default. Once
// the consumer goroutine drops its reference, Go's GC reclaims the
// channel.
func (l *Log) Unsubscribe(ch chan Event) {
	l.mu.Lock()
	for i, c := range l.listeners {
		if c == ch {
			l.listeners = append(l.listeners[:i], l.listeners[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
}

// ─────────────────────────── convenience constructors ───────────────────────────
// These are sugar so callers don't have to spell out Event{} fields.

func Started(tunnel string) Event {
	return Event{Type: "started", Level: Info, Tunnel: tunnel, Message: tunnel + " started"}
}
func Stopped(tunnel string) Event {
	return Event{Type: "stopped", Level: Info, Tunnel: tunnel, Message: tunnel + " stopped"}
}
func Restarted(tunnel string) Event {
	return Event{Type: "restarted", Level: Info, Tunnel: tunnel, Message: tunnel + " restarted"}
}
func Created(tunnel, mode, transport string) Event {
	return Event{Type: "created", Level: Info, Tunnel: tunnel, Message: tunnel + " created (" + mode + "/" + transport + ")"}
}
func Updated(tunnel string) Event {
	return Event{Type: "updated", Level: Info, Tunnel: tunnel, Message: tunnel + " config updated"}
}
func Deleted(tunnel string) Event {
	return Event{Type: "deleted", Level: Warn, Tunnel: tunnel, Message: tunnel + " deleted"}
}
func Failed(tunnel, reason string) Event {
	return Event{Type: "failed", Level: Error, Tunnel: tunnel, Message: tunnel + " failed: " + reason}
}
func StateChange(tunnel, from, to string) Event {
	level := Info
	if to == "failed" {
		level = Error
	} else if to == "inactive" && from == "active" {
		level = Warn
	}
	return Event{Type: "state_change", Level: level, Tunnel: tunnel, Message: tunnel + ": " + from + " → " + to}
}

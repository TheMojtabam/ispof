// Package scraper polls each tunnel's Prometheus /metrics endpoint and
// keeps a rolling ring buffer of samples per tunnel. It is the data
// source for every chart on the dashboard.
//
// The Prometheus text format parser is written by hand rather than
// pulling in github.com/prometheus/common/expfmt — the format is simple
// (line-oriented, name[{labels}] value) and avoiding the dep keeps the
// binary single-file and easy to audit.
//
// Scrapes happen concurrently across tunnels with a per-tunnel timeout
// so one slow endpoint can't delay the loop.
package scraper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────── public types ───────────────────────────

// Sample is a single timestamped scrape result. Metrics keys are in
// the canonical "name{a=\"x\",b=\"y\"}" form with labels alpha-sorted
// so the same logical series always has the same key across scrapes.
//
// Up — the synthetic "did this scrape succeed" gauge — follows the
// Prometheus convention: 1.0 = healthy, 0.0 = failed. Operators can
// graph it directly.
type Sample struct {
	Time    time.Time          `json:"time"`
	Up      float64            `json:"up"`               // 1.0 if scrape succeeded, 0.0 otherwise
	Error   string             `json:"error,omitempty"`  // human-readable failure reason
	Metrics map[string]float64 `json:"metrics"`          // empty on failed scrape
}

// Config tunes the scraper. Zero values get sensible defaults.
type Config struct {
	Interval   time.Duration              // default: 5s
	Timeout    time.Duration              // default: 3s (per-target HTTP timeout)
	MaxHistory int                        // default: 60 samples ≈ 5 min @ 5s
	Logf       func(string, ...any)       // optional, defaults to silent
}

// Scraper holds the rolling history. Safe for concurrent use.
type Scraper struct {
	cfg    Config
	client *http.Client

	mu      sync.RWMutex
	history map[string][]Sample // tunnel name → ring buffer (chronological)

	// onSample notifies subscribers (currently: state-change detector)
	// each time a new sample lands. nil means no notification.
	onSample func(tunnel string, s Sample)
}

// TargetsFn returns the current map of {tunnel name → metrics URL}.
// Empty URL means "skip this tunnel". The scraper calls TargetsFn at
// the top of every tick so adding/removing tunnels at runtime works
// without a restart.
type TargetsFn func() map[string]string

func New(cfg Config) *Scraper {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.MaxHistory == 0 {
		cfg.MaxHistory = 60
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Scraper{
		cfg:     cfg,
		client:  &http.Client{Timeout: cfg.Timeout},
		history: make(map[string][]Sample),
	}
}

// OnSample registers a callback for every successful or failed scrape.
// Called synchronously after the sample is pushed to history; the
// callback should be cheap (push to a channel, push to event log) and
// must not block.
func (s *Scraper) OnSample(fn func(tunnel string, sample Sample)) {
	s.onSample = fn
}

// Latest returns the most recent sample for a tunnel, or (zero, false)
// if no scrape has completed yet.
func (s *Scraper) Latest(name string) (Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hist := s.history[name]
	if len(hist) == 0 {
		return Sample{}, false
	}
	return hist[len(hist)-1], true
}

// History returns a copy of the entire ring buffer (chronological,
// oldest first). The dashboard's sparkline uses this for the time axis.
func (s *Scraper) History(name string) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hist := s.history[name]
	out := make([]Sample, len(hist))
	copy(out, hist)
	return out
}

// AllHistory returns a snapshot of every tunnel's history. Useful for
// the global metrics view.
func (s *Scraper) AllHistory() map[string][]Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Sample, len(s.history))
	for name, hist := range s.history {
		c := make([]Sample, len(hist))
		copy(c, hist)
		out[name] = c
	}
	return out
}

// Rate returns the per-second rate of a counter metric computed from
// the last two samples. Returns 0 for a counter reset (delta < 0) or
// if there aren't yet two samples to compare.
func (s *Scraper) Rate(name, metric string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hist := s.history[name]
	if len(hist) < 2 {
		return 0
	}
	a, b := hist[len(hist)-2], hist[len(hist)-1]
	if a.Up == 0 || b.Up == 0 {
		return 0
	}
	dt := b.Time.Sub(a.Time).Seconds()
	if dt <= 0 {
		return 0
	}
	d := b.Metrics[metric] - a.Metrics[metric]
	if d < 0 {
		return 0
	}
	return d / dt
}

// Start runs the scrape loop until ctx is cancelled. The targets
// callback is invoked at the start of every tick to learn the current
// tunnels — this way changes from the API surface immediately, no
// reload required.
func (s *Scraper) Start(ctx context.Context, targets TargetsFn) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	// Eager first tick so the dashboard has data within seconds of boot.
	s.scrapeAll(ctx, targets())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scrapeAll(ctx, targets())
		}
	}
}

func (s *Scraper) scrapeAll(ctx context.Context, targets map[string]string) {
	if len(targets) == 0 {
		return
	}
	var wg sync.WaitGroup
	for name, url := range targets {
		if url == "" {
			continue
		}
		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			sample := s.scrapeOne(ctx, url)
			s.push(name, sample)
		}(name, url)
	}
	wg.Wait()
}

// scrapeOne performs one HTTP GET and parses the response. Failures
// don't return errors — they produce Sample{Up: 0, Error: ...} so the
// history still gets a data point and the UI can show "scraping
// failing" instead of "no data".
func (s *Scraper) scrapeOne(ctx context.Context, url string) Sample {
	sample := Sample{Time: time.Now(), Metrics: map[string]float64{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		sample.Error = fmt.Sprintf("build request: %v", err)
		return sample
	}
	resp, err := s.client.Do(req)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "connection refused"):
			sample.Error = "metrics endpoint refused connection (daemon not running?)"
		case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "Timeout"):
			sample.Error = "metrics endpoint timed out"
		default:
			sample.Error = msg
		}
		return sample
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		sample.Error = fmt.Sprintf("HTTP %d from metrics endpoint", resp.StatusCode)
		return sample
	}
	metrics, err := Parse(resp.Body)
	if err != nil {
		sample.Error = fmt.Sprintf("parse: %v", err)
		return sample
	}
	sample.Up = 1.0
	sample.Metrics = metrics
	return sample
}

func (s *Scraper) push(name string, sample Sample) {
	s.mu.Lock()
	hist := s.history[name]
	hist = append(hist, sample)
	if len(hist) > s.cfg.MaxHistory {
		hist = hist[len(hist)-s.cfg.MaxHistory:]
	}
	s.history[name] = hist
	cb := s.onSample
	s.mu.Unlock()

	if cb != nil {
		cb(name, sample)
	}
	if sample.Up == 0 {
		s.cfg.Logf("scrape %s failed: %s", name, sample.Error)
	}
}

// ─────────────────────────── parser ───────────────────────────

// Parse reads Prometheus text-format output and returns a flat map of
// canonical-key → value.
//
// Canonical key form: name{labelA="v",labelB="v"} with labels
// alpha-sorted so identical series always hash the same way regardless
// of label order on the wire.
//
// Comments (# HELP / # TYPE) and blank lines are skipped. Malformed
// lines are silently skipped — one bad metric should never poison the
// rest of the scrape.
func Parse(r io.Reader) (map[string]float64, error) {
	out := make(map[string]float64)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := parseLine(line)
		if !ok {
			continue
		}
		out[name] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseLine(line string) (string, float64, bool) {
	// metric_name{labels} value [optional_timestamp]
	// The split happens at the first space outside any {} block.
	var (
		depth   int
		nameEnd = -1
	)
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ' ':
			if depth == 0 {
				nameEnd = i
			}
		}
		if nameEnd >= 0 {
			break
		}
	}
	if nameEnd <= 0 {
		return "", 0, false
	}
	metricPart := line[:nameEnd]
	rest := strings.TrimSpace(line[nameEnd+1:])
	if sp := strings.IndexByte(rest, ' '); sp > 0 {
		rest = rest[:sp]
	}
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return "", 0, false
	}
	name, labels, hasLabels := splitNameLabels(metricPart)
	if !hasLabels {
		return name, v, true
	}
	return name + "{" + canonicalizeLabels(labels) + "}", v, true
}

func splitNameLabels(s string) (name, labels string, ok bool) {
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return s, "", false
	}
	if !strings.HasSuffix(s, "}") {
		return s, "", false
	}
	return s[:i], s[i+1 : len(s)-1], true
}

// canonicalizeLabels sorts label pairs by name. We use a naïve
// comma-split: Prometheus label values are quoted and escaped so they
// don't contain raw commas per the spec. For our purposes this is fine.
func canonicalizeLabels(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ",")
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j-1] > parts[j]; j-- {
			parts[j-1], parts[j] = parts[j], parts[j-1]
		}
	}
	return strings.Join(parts, ",")
}

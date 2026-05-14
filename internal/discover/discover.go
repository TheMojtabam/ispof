// Package discover finds quiccochet tunnel configurations that exist
// on the server outside of /etc/ispof/tunnels/. Users frequently have
// configs they created by hand (or via the upstream quiccochet install
// script) in random places like /root/server.json or /etc/quiccochet/.
// The panel lets them import those into the store with one click.
//
// Three discovery strategies, in order of decreasing reliability:
//
//   1. Running processes — `ps` for `quiccochet -c <path>`
//   2. Systemd unit files — parse ExecStart= for -c flags
//   3. Filesystem scan — find *.json that look like a quiccochet config
//
// Each strategy reports the config file path; the caller is responsible
// for reading it and deciding whether to import.
package discover

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Hit describes one discovered tunnel config.
type Hit struct {
	Path   string `json:"path"`             // absolute filesystem path
	Source string `json:"source"`           // "process" | "systemd" | "filesystem"
	Name   string `json:"name"`             // derived from filename or content
	Mode   string `json:"mode,omitempty"`   // client | server
	Transport string `json:"transport,omitempty"`
	HasKey bool   `json:"has_key,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Options tunes the search. Zero values get sensible defaults.
type Options struct {
	// ExcludeDir is the panel's own tunnels directory — paths inside
	// it are skipped to avoid surfacing tunnels we already manage.
	ExcludeDir string

	// FilesystemBudget caps the slow filesystem scan. Default 30s.
	FilesystemBudget time.Duration

	// KnownDirs is the priority list of directories to scan first.
	// Defaults are derived from where users + the upstream install
	// script typically write configs.
	KnownDirs []string
}

func (o *Options) defaults() {
	if o.FilesystemBudget == 0 {
		o.FilesystemBudget = 30 * time.Second
	}
	if len(o.KnownDirs) == 0 {
		o.KnownDirs = []string{
			"/etc/quiccochet",
			"/etc/quiccochet/configs",
			"/etc/quiccochet/tunnels",
			"/opt/quiccochet",
			"/opt/quiccochet/configs",
			"/usr/local/etc/quiccochet",
			"/var/lib/quiccochet",
			"/srv/quiccochet",
			"/root",
			"/root/configs",
			"/root/quiccochet",
		}
		// Also add per-user homes — globbed at run time.
		matches, _ := filepath.Glob("/home/*")
		for _, h := range matches {
			o.KnownDirs = append(o.KnownDirs,
				h, filepath.Join(h, "configs"), filepath.Join(h, "quiccochet"))
		}
	}
}

// Discover runs all strategies and returns a deduplicated, ordered
// list of hits. Process / systemd hits come first because they prove
// a tunnel is actively in use; filesystem hits come last and may
// include orphan configs.
func Discover(ctx context.Context, opt Options) []Hit {
	opt.defaults()
	seen := make(map[string]bool)
	out := []Hit{}

	addHit := func(h Hit) {
		if h.Path == "" {
			return
		}
		// Resolve symlinks so /etc/X and /etc/X/Y/../X show up as one.
		abs, err := filepath.Abs(h.Path)
		if err == nil {
			h.Path = abs
		}
		// Skip the panel's own dir — those are already managed.
		if opt.ExcludeDir != "" && strings.HasPrefix(h.Path, opt.ExcludeDir+"/") {
			return
		}
		if seen[h.Path] {
			return
		}
		seen[h.Path] = true
		// Enrich with quick metadata.
		fillMeta(&h)
		if h.Mode == "" {
			// not actually a quiccochet config — drop
			delete(seen, h.Path)
			return
		}
		out = append(out, h)
	}

	for _, h := range fromProcesses(ctx) {
		addHit(h)
	}
	for _, h := range fromSystemd(ctx) {
		addHit(h)
	}
	for _, h := range fromFilesystem(ctx, opt) {
		addHit(h)
	}
	return out
}

// fromProcesses parses `ps -eo args=` for any process whose argv looks
// like "quiccochet ... -c <path>". This finds tunnels that were started
// outside of systemd (e.g. tmux, nohup, screen).
func fromProcesses(ctx context.Context) []Hit {
	out := []Hit{}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "ps", "-eo", "args=")
	bytes, err := cmd.Output()
	if err != nil {
		return out
	}
	re := regexp.MustCompile(`quiccochet\b[^\n]*?-c\s+(\S+)`)
	for _, line := range strings.Split(string(bytes), "\n") {
		m := re.FindStringSubmatch(line)
		if len(m) >= 2 {
			out = append(out, Hit{Path: m[1], Source: "process"})
		}
	}
	return out
}

// fromSystemd parses ExecStart= of every unit whose name starts with
// "quiccochet". Works for both regular and template units.
func fromSystemd(ctx context.Context) []Hit {
	out := []Hit{}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// list-unit-files gives us both running and disabled units
	cmd := exec.CommandContext(c, "systemctl", "list-unit-files", "--no-legend", "--no-pager", "quiccochet*")
	listBytes, err := cmd.Output()
	if err != nil {
		return out
	}
	units := []string{}
	for _, line := range strings.Split(string(listBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".service") {
			units = append(units, fields[0])
		}
	}
	for _, u := range units {
		// Use `systemctl cat` instead of show to get the actual ExecStart
		// argv text rather than the {path=/...; argv=[]} struct form.
		c2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		out2, err := exec.CommandContext(c2, "systemctl", "cat", u).Output()
		cancel2()
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`ExecStart=[^\n]*-c\s+(\S+)`)
		m := re.FindStringSubmatch(string(out2))
		if len(m) >= 2 {
			out = append(out, Hit{Path: m[1], Source: "systemd"})
		}
	}
	return out
}

// fromFilesystem does a bounded scan of common directories + a fallback
// `find` for anything matching a quiccochet-shaped .json file.
//
// Strategy:
//   1. Scan KnownDirs (instant — direct readdir, no recursion)
//   2. Run `find / -xdev -name '*.json'` within the budget
//   3. For each candidate, peek the first few KB looking for the
//      characteristic "mode": "client"/"server" + "transport" keys.
func fromFilesystem(ctx context.Context, opt Options) []Hit {
	out := []Hit{}
	visit := func(path string, source string) {
		if !looksLikeQuiccochetConfig(path) {
			return
		}
		out = append(out, Hit{Path: path, Source: source})
	}

	// 1. Known dirs (shallow scan, instant).
	for _, dir := range opt.KnownDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				visit(filepath.Join(dir, e.Name()), "filesystem")
			}
		}
	}
	// And one level deeper in case configs live in /etc/quiccochet/foo/
	for _, dir := range opt.KnownDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				sub := filepath.Join(dir, e.Name())
				subEntries, _ := os.ReadDir(sub)
				for _, e2 := range subEntries {
					if !e2.IsDir() && strings.HasSuffix(e2.Name(), ".json") {
						visit(filepath.Join(sub, e2.Name()), "filesystem")
					}
				}
			}
		}
	}

	// 2. Whole-filesystem scan within the budget (skip if we found
	// plenty already to avoid wasting time on a large server).
	if len(out) < 10 {
		c, cancel := context.WithTimeout(ctx, opt.FilesystemBudget)
		defer cancel()
		cmd := exec.CommandContext(c, "find", "/", "-xdev", "-type", "f",
			"-name", "*.json", "-size", "+100c", "-size", "-200k")
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			_ = cmd.Start()
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				p := sc.Text()
				// Cheap path filter — skip virtual filesystems and noise
				if strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/") ||
					strings.HasPrefix(p, "/run/") || strings.HasPrefix(p, "/var/lib/docker/") ||
					strings.HasPrefix(p, "/var/lib/containerd/") ||
					strings.Contains(p, "/node_modules/") || strings.Contains(p, "/.cache/") {
					continue
				}
				visit(p, "filesystem")
			}
			_ = cmd.Wait()
		}
	}
	return out
}

// LooksLikeQuiccochetConfig peeks the first 4 KB of a file and returns
// true if it contains the characteristic JSON keys. We don't fully
// parse — that's expensive — we just look for the markers.
//
// Exported so other packages can use it as an authorization gate
// before reading a caller-supplied path: a request to "import this
// file" is only allowed if the file looks like a quiccochet config,
// preventing the import endpoint from being abused to read arbitrary
// files like /etc/shadow.
func LooksLikeQuiccochetConfig(path string) bool {
	return looksLikeQuiccochetConfig(path)
}

// looksLikeQuiccochetConfig peeks the first 4 KB of a file and returns
// true if it contains the characteristic JSON keys. We don't fully
// parse — that's expensive — we just look for the markers.
func looksLikeQuiccochetConfig(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() > 200_000 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	head := string(buf[:n])
	// Quick markers: must contain "transport" and either "mode" or
	// "spoof" or "crypto". This rejects unrelated JSON files like
	// package-lock.json, tsconfig.json, etc.
	if !strings.Contains(head, `"transport"`) {
		return false
	}
	if !strings.Contains(head, `"mode"`) &&
		!strings.Contains(head, `"spoof"`) &&
		!strings.Contains(head, `"crypto"`) {
		return false
	}
	return true
}

// fillMeta does a real JSON parse of the file to extract human-useful
// metadata (mode, transport type, key presence) for the UI.
func fillMeta(h *Hit) {
	data, err := os.ReadFile(h.Path)
	if err != nil {
		return
	}
	h.Size = int64(len(data))
	var probe struct {
		Mode      string `json:"mode"`
		Transport struct {
			Type string `json:"type"`
		} `json:"transport"`
		Crypto struct {
			PrivateKey    string `json:"private_key"`
			PeerPublicKey string `json:"peer_public_key"`
		} `json:"crypto"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	h.Mode = probe.Mode
	h.Transport = probe.Transport.Type
	h.HasKey = probe.Crypto.PrivateKey != ""

	// Best-effort name: use the filename without extension.
	base := filepath.Base(h.Path)
	h.Name = strings.TrimSuffix(base, filepath.Ext(base))
}

// ReadConfig loads a discovered config from its absolute path, returning
// raw bytes ready to be unmarshaled into store.Tunnel. Callers should
// pass the result through their own validator before importing.
func ReadConfig(path string) ([]byte, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("path must be absolute: %s", path)
	}
	return os.ReadFile(clean)
}

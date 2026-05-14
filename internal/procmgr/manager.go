// Package procmgr drives tunnel lifecycle through systemd template units.
//
// Each tunnel name X maps to the systemd unit `quiccochet@X.service`,
// which is a template unit installed by the panel's installer that reads
// /etc/ispof/tunnels/X.json and execs the quiccochet binary.
//
// We deliberately shell out to systemctl rather than dialing systemd's
// D-Bus interface. The trade-off is one extra fork per call vs not
// pulling in dbus libraries (which were the largest dependency in the
// previous draft). For a panel that handles single-digit ops/min this
// is the right call.
package procmgr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// UnitTemplate is the systemd template unit name. The "@" makes it an
// instance template; "%i" inside the unit file expands to the part
// between "@" and ".service".
const UnitTemplate = "quiccochet@"

// State describes a tunnel from systemd's perspective.
type State struct {
	Name        string `json:"name"`
	ActiveState string `json:"active_state"` // active | inactive | failed | activating | deactivating
	SubState    string `json:"sub_state"`    // running | dead | failed | start-pre ...
	Pid         int    `json:"pid,omitempty"`
	MainPID     string `json:"main_pid_raw,omitempty"`
	MemoryRSS   uint64 `json:"memory_rss,omitempty"` // bytes
	Tasks       int    `json:"tasks,omitempty"`
	SinceUnix   int64  `json:"since_unix,omitempty"` // ActiveEnterTimestamp
	ExecMain    string `json:"exec_main_status,omitempty"`

	// ExternalPid is set when we find a quiccochet process running with
	// this tunnel's config path that ISN'T managed by our systemd template
	// unit — i.e. the user started quiccochet by hand, or via the upstream
	// install script's own unit. The panel surfaces this so the user can
	// see the tunnel IS running even though `quiccochet@X.service` says
	// inactive, and can decide whether to take it over.
	ExternalPid int    `json:"external_pid,omitempty"`
	ExternalCmd string `json:"external_cmd,omitempty"`
}

// Manager wraps systemctl invocations. The zero value is unusable;
// construct with New().
type Manager struct {
	systemctl string // path to systemctl binary (allows tests to substitute a fake)
	timeout   time.Duration
}

// New returns a Manager that uses the system's `systemctl`. The look-up
// happens once at construction time so we fail fast on systems without
// systemd rather than per-call.
func New() (*Manager, error) {
	p, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, fmt.Errorf("systemctl not found in PATH: %w", err)
	}
	return &Manager{systemctl: p, timeout: 10 * time.Second}, nil
}

func (m *Manager) unitName(tunnel string) string {
	return UnitTemplate + tunnel + ".service"
}

// Start begins a tunnel. systemctl returns synchronously once the unit
// has been "started" by its rules (which for Type=simple means the binary
// has been exec'd; for Type=notify it means the binary has signalled
// READY=1).
func (m *Manager) Start(tunnel string) error {
	return m.run("start", m.unitName(tunnel))
}

// Stop sends SIGTERM and waits for systemd to acknowledge.
func (m *Manager) Stop(tunnel string) error {
	return m.run("stop", m.unitName(tunnel))
}

// Restart is a stop+start atomically from the user's POV.
func (m *Manager) Restart(tunnel string) error {
	return m.run("restart", m.unitName(tunnel))
}

// Enable persists the unit so it starts at boot.
func (m *Manager) Enable(tunnel string) error {
	return m.run("enable", m.unitName(tunnel))
}

// Disable removes the boot-time link without stopping a running instance.
func (m *Manager) Disable(tunnel string) error {
	return m.run("disable", m.unitName(tunnel))
}

// IsActive returns true if the unit is in the "active" state. Any other
// state including "failed" returns false. Errors are returned only for
// systemctl-level failures (e.g. systemd not running).
func (m *Manager) IsActive(tunnel string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.systemctl, "is-active", m.unitName(tunnel))
	out, err := cmd.Output()
	state := strings.TrimSpace(string(out))
	// `systemctl is-active` exits non-zero for any non-active state but
	// still prints the state name, so we treat "process exited" as
	// information rather than failure.
	if state == "active" {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// State fetches the full state via `systemctl show` parsing key=value
// output. Returns zero-value State and nil error for unknown units.
func (m *Manager) State(tunnel string) (State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	props := []string{
		"ActiveState", "SubState", "MainPID", "MemoryCurrent",
		"TasksCurrent", "ActiveEnterTimestampMonotonic", "ExecMainStatus",
		"ActiveEnterTimestamp",
	}
	args := []string{"show", m.unitName(tunnel), "--no-page", "--property=" + strings.Join(props, ",")}
	cmd := exec.CommandContext(ctx, m.systemctl, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// systemctl writes "Failed to get unit" to stderr for unknown
			// units and exits 0 sometimes / non-zero other times depending
			// on version. Either way: empty state is the right answer.
			return State{Name: tunnel}, nil
		}
		return State{}, err
	}
	s := State{Name: tunnel}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			s.ActiveState = v
		case "SubState":
			s.SubState = v
		case "MainPID":
			s.MainPID = v
			fmt.Sscanf(v, "%d", &s.Pid)
		case "MemoryCurrent":
			fmt.Sscanf(v, "%d", &s.MemoryRSS)
		case "TasksCurrent":
			fmt.Sscanf(v, "%d", &s.Tasks)
		case "ActiveEnterTimestamp":
			if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", v); err == nil {
				s.SinceUnix = t.Unix()
			}
		case "ExecMainStatus":
			s.ExecMain = v
		}
	}
	return s, nil
}

// FindExternal scans the process table for a quiccochet process whose
// argv contains "-c <configPath>" or "-c <anything containing tunnelName>".
// Returns 0/"" if nothing matches. This is how we surface tunnels that
// are running outside our systemd template unit — typically because the
// user had them set up manually before installing the panel.
//
// We accept the broader "tunnelName match" because the user often imports
// a config from /root/foo.json into /etc/ispof/tunnels/foo.json — the
// external process is still using /root/foo.json, but the FILE NAMES
// match so the panel can confidently report "running externally".
func (m *Manager) FindExternal(ctx context.Context, tunnel, configPath string) (pid int, cmdline string) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "ps", "-eo", "pid,args").Output()
	if err != nil {
		return 0, ""
	}
	// Build the patterns we'll look for. The exact config path is the
	// strongest signal; the tunnel name (matching foo.json suffix) is the
	// fallback.
	wantPath := configPath
	wantName := "/" + tunnel + ".json"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Lines look like:  "  1234 /usr/local/bin/quiccochet -c /etc/x.json"
		if !strings.Contains(line, "quiccochet") {
			continue
		}
		if !strings.Contains(line, " -c ") && !strings.Contains(line, "\t-c\t") {
			continue
		}
		// Path match
		matched := false
		if wantPath != "" && strings.Contains(line, " -c "+wantPath) {
			matched = true
		} else if wantPath != "" && strings.Contains(line, " -c "+wantPath+" ") {
			matched = true
		} else if strings.Contains(line, wantName) {
			matched = true
		}
		if !matched {
			continue
		}
		// Parse PID (first whitespace-separated field).
		var p int
		fmt.Sscanf(line, "%d", &p)
		// Skip our own systemd-managed process if it's been started — we
		// don't want to claim it's "external". Detect by checking whether
		// the cmdline contains /etc/ispof/tunnels/.
		if strings.Contains(line, "/etc/ispof/tunnels/") {
			continue
		}
		return p, line
	}
	return 0, ""
}


// For a streaming view callers should use TailLogs instead.
func (m *Manager) Logs(tunnel string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", m.unitName(tunnel),
		"-n", fmt.Sprintf("%d", lines),
		"--no-pager",
		"--output=short-iso",
	)
	out, err := cmd.Output()
	return string(out), err
}

// TailLogs returns a running journalctl process bound to ctx; reading
// stdout yields new log lines as they arrive. Cancel ctx to stop.
func (m *Manager) TailLogs(ctx context.Context, tunnel string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", m.unitName(tunnel),
		"-f",                  // follow
		"-n", "50",            // back-fill so the client gets context
		"--no-pager",
		"--output=short-iso",
	)
	return cmd, nil
}

// DaemonReload prods systemd to re-read unit files. Needed once after
// the panel installs or removes a template unit file.
func (m *Manager) DaemonReload() error {
	return m.run("daemon-reload")
}

func (m *Manager) run(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.systemctl, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// surface stderr so the API can return something useful
		return fmt.Errorf("systemctl %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

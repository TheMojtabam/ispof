// Package sys collects host metrics and applies kernel + firewall tuning.
package sys

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const TuningPath = "/etc/sysctl.d/99-quiccochet.conf"

// Sysctl is a value-typed helper around procfs.
type Sysctl struct {
	lastCPU atomic.Value // cpuSnap
}

type cpuSnap struct {
	idle, total uint64
	at          time.Time
}

func NewSysctl() *Sysctl { return &Sysctl{} }

// Kernel returns "uname -r"
func (*Sysctl) Kernel() string {
	out, _ := exec.Command("uname", "-r").Output()
	return strings.TrimSpace(string(out))
}

func (*Sysctl) OSRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return runtime.GOOS
}

func (*Sysctl) UptimeSec() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(parts[0], 64)
	return v
}

// ResourceSnapshot returns CPU%, RAM used MB, RAM total MB.
func (s *Sysctl) ResourceSnapshot() (cpu, ramUsed, ramTotal float64) {
	cpu = s.cpuPercent()
	ramUsed, ramTotal = s.memMB()
	return
}

func (s *Sysctl) cpuPercent() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 {
		return 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 8 {
		return 0
	}
	var idle, total uint64
	for i := 1; i < 8; i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	now := cpuSnap{idle: idle, total: total, at: time.Now()}
	prev, _ := s.lastCPU.Load().(cpuSnap)
	s.lastCPU.Store(now)
	if prev.total == 0 || now.total <= prev.total {
		return 0
	}
	totalDelta := now.total - prev.total
	idleDelta := now.idle - prev.idle
	return float64(totalDelta-idleDelta) * 100.0 / float64(totalDelta)
}

func (*Sysctl) memMB() (used, total float64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTot, memAvail float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			memTot = v / 1024
		case "MemAvailable:":
			memAvail = v / 1024
		}
	}
	return memTot - memAvail, memTot
}

func (*Sysctl) DiskUsedPct(path string) float64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bfree * uint64(st.Bsize)
	used := total - free
	if total == 0 {
		return 0
	}
	return float64(used) * 100.0 / float64(total)
}

func (*Sysctl) NetIfaces() []string {
	out := []string{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		out = append(out, i.Name)
	}
	return out
}

// ---- Sysctl tuning ----

// DefaultTuning returns the recommended sysctl values for the tunnel.
func (*Sysctl) DefaultTuning() map[string]string {
	return map[string]string{
		"net.core.rmem_max":               "134217728",
		"net.core.wmem_max":               "134217728",
		"net.core.rmem_default":           "2097152",
		"net.core.wmem_default":           "2097152",
		"net.core.netdev_max_backlog":     "30000",
		"net.core.somaxconn":              "4096",
		"net.core.default_qdisc":          "fq",
		"net.ipv4.tcp_congestion_control": "bbr",
		"net.ipv4.tcp_fastopen":           "3",
		"net.ipv4.tcp_mtu_probing":        "1",
		"net.ipv4.udp_mem":                "102400 873800 134217728",
		"net.netfilter.nf_conntrack_max":  "1048576",
		"net.ipv4.ip_forward":             "1",
	}
}

// GetTuning reads each tuned key from /proc/sys.
func (s *Sysctl) GetTuning() map[string]string {
	out := map[string]string{}
	for k := range s.DefaultTuning() {
		path := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
		if b, err := os.ReadFile(path); err == nil {
			out[k] = strings.TrimSpace(string(b))
		}
	}
	return out
}

// ApplyTuning writes the defaults to TuningPath and runs sysctl -p.
func (s *Sysctl) ApplyTuning() error {
	def := s.DefaultTuning()
	var sb strings.Builder
	sb.WriteString("# QUICochet — applied by panel\n")
	for k, v := range def {
		fmt.Fprintf(&sb, "%s=%s\n", k, v)
	}
	if err := os.WriteFile(TuningPath, []byte(sb.String()), 0644); err != nil {
		return err
	}
	out, err := exec.Command("sysctl", "-p", TuningPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl -p: %v: %s", err, out)
	}
	return nil
}

// ---- Firewall ----

func (*Sysctl) FirewallStatus() ([]string, error) {
	if _, err := exec.LookPath("ufw"); err == nil {
		out, _ := exec.Command("ufw", "status", "numbered").CombinedOutput()
		lines := []string{}
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return lines, nil
	}
	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		out, _ := exec.Command("firewall-cmd", "--list-all").CombinedOutput()
		return []string{string(out)}, nil
	}
	out, _ := exec.Command("iptables", "-S").CombinedOutput()
	return []string{string(out)}, nil
}

// FirewallAdd parses simple "allow 443/tcp" style rules. For complex rules
// edit the OS firewall directly.
func (*Sysctl) FirewallAdd(rule string) error {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return fmt.Errorf("empty rule")
	}
	if _, err := exec.LookPath("ufw"); err == nil {
		args := append([]string{"allow"}, strings.Fields(rule)...)
		out, err := exec.Command("ufw", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ufw: %v: %s", err, out)
		}
		return nil
	}
	return fmt.Errorf("no supported firewall found")
}

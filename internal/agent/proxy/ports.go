package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// Conflict is a loopback address the proxy needs and someone else listens on.
type Conflict struct {
	Addr netip.AddrPort
	// Owner is the listening program as lsof/ss name it; empty if it cannot be seen
	// (another user's process without sudo).
	Owner string
	PID   int
	// Kind is a known dev server (nginx, httpd, caddy, herd) that can coexist, or the
	// container runtime when one of its containers publishes the port.
	Kind string
}

func (c Conflict) String() string {
	owner := c.Owner
	if owner == "" {
		owner = "an unknown program"
	} else if c.PID > 0 {
		owner = fmt.Sprintf("%s (pid %d)", owner, c.PID)
	}
	return c.Addr.String() + " is taken by " + owner
}

// Hint says what the person can do about the conflict.
func (c Conflict) Hint() string {
	port := c.Addr.Port()
	switch {
	case c.Kind == "runtime":
		return fmt.Sprintf("a container publishes port %d: docker ps --filter publish=%d", port, port)
	case c.Kind != "":
		return fmt.Sprintf("move %s to 8080/8443, then 'devcorb proxy coexist --http 8080 --https 8443': Devcorb takes 80/443 and passes unknown hosts to it", c.Kind)
	case c.Owner == "":
		return fmt.Sprintf("sudo lsof -nP -iTCP:%d -sTCP:LISTEN shows the owner", port)
	}
	return fmt.Sprintf("stop %s or leave Devcorb on the fallback ports", c.Owner)
}

// Taken probes the loopback addresses of ports and returns those someone listens on.
// It connects rather than binds: binding 127.0.0.1:80 needs root on macOS (P-6).
func Taken(ctx context.Context, ports Ports) []Conflict {
	var out []Conflict
	for _, port := range []uint16{ports.HTTP, ports.HTTPS} {
		for _, ip := range Loopback {
			ap := netip.AddrPortFrom(ip, port)
			if !listening(ctx, ap) {
				continue
			}
			c := Conflict{Addr: ap}
			c.Owner, c.PID = owner(ctx, ap)
			c.Kind = kindOf(c.Owner)
			out = append(out, c)
		}
	}
	return out
}

func listening(ctx context.Context, ap netip.AddrPort) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ap.String())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ChoosePorts picks the default ports if both loopback families are free on both,
// otherwise the fallback ports (local-networking.md §1). The conflicts explain why.
func ChoosePorts(ctx context.Context) (Ports, []Conflict, error) {
	conflicts := Taken(ctx, DefaultPorts)
	if len(conflicts) == 0 {
		return DefaultPorts, nil, nil
	}
	if more := Taken(ctx, FallbackPorts); len(more) > 0 {
		all := append(conflicts, more...)
		return Ports{}, all, fmt.Errorf("ports 80/443 and the fallback %d/%d are taken: %s", FallbackPorts.HTTP, FallbackPorts.HTTPS, more[0])
	}
	return FallbackPorts, conflicts, nil
}

// owner finds the listener with lsof (macOS, Linux) and falls back to ss on Linux.
func owner(ctx context.Context, ap netip.AddrPort) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	port := strconv.Itoa(int(ap.Port()))
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fpcn").Output()
	if err == nil {
		if name, pid := parseLsof(string(out), ap); name != "" {
			return name, pid
		}
	}
	if goruntime.GOOS == "linux" {
		out, err := exec.CommandContext(ctx, "ss", "-Hltnp", "sport = :"+port).Output()
		if err == nil {
			return parseSS(string(out))
		}
	}
	return "", 0
}

// parseLsof reads `lsof -Fpcn`: p<pid>, c<command>, then n<address> per socket. It
// prefers the process listening on the exact address, then a wildcard listener.
func parseLsof(out string, ap netip.AddrPort) (string, int) {
	var pid int
	var cmd string
	exact, wild := [2]any{}, [2]any{}
	want := ap.String()
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			cmd = line[1:]
		case 'n':
			addr := line[1:]
			switch {
			case addr == want && exact[0] == nil:
				exact = [2]any{cmd, pid}
			case (strings.HasPrefix(addr, "*:") || strings.HasPrefix(addr, "[::]:") || strings.HasPrefix(addr, "0.0.0.0:")) && wild[0] == nil:
				wild = [2]any{cmd, pid}
			}
		}
	}
	for _, m := range [][2]any{exact, wild} {
		if m[0] != nil {
			return m[0].(string), m[1].(int)
		}
	}
	return "", 0
}

// parseSS reads `ss -Hltnp`: users:(("nginx",pid=812,fd=6)).
func parseSS(out string) (string, int) {
	i := strings.Index(out, `users:(("`)
	if i < 0 {
		return "", 0
	}
	rest := out[i+len(`users:(("`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", 0
	}
	name := rest[:j]
	pid := 0
	if k := strings.Index(rest, "pid="); k >= 0 {
		end := strings.IndexAny(rest[k+4:], ",)")
		if end > 0 {
			pid, _ = strconv.Atoi(rest[k+4 : k+4+end])
		}
	}
	return name, pid
}

// kindOf recognizes dev servers that can move off 80/443 and container runtimes.
func kindOf(owner string) string {
	o := strings.ToLower(owner)
	switch {
	case o == "":
		return ""
	case strings.Contains(o, "com.docker"), strings.Contains(o, "vpnkit"), strings.Contains(o, "orbstack"),
		strings.Contains(o, "limactl"), strings.Contains(o, "docker-proxy"), strings.Contains(o, "gvproxy"), strings.Contains(o, "rootlessport"):
		return "runtime"
	case strings.Contains(o, "herd"):
		return "Laravel Herd"
	case strings.Contains(o, "nginx"):
		return "nginx"
	case strings.Contains(o, "httpd"), strings.Contains(o, "apache"):
		return "Apache"
	case strings.Contains(o, "caddy"):
		return "Caddy"
	}
	return ""
}

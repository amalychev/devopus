package main

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Port modes from docs/architecture/local-networking.md: default (80/443) when free,
// otherwise an explicit fallback to fixed high ports. Foreign services are never stopped.

const (
	fallbackHTTP  = 18080
	fallbackHTTPS = 18443
)

type portCheck struct {
	Addr  string
	Busy  bool
	Owner string
}

func checkPort(host string, port int) portCheck {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	pc := portCheck{Addr: addr}
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return pc
	}
	conn.Close()
	pc.Busy = true
	pc.Owner = portOwner(port)
	return pc
}

// portOwner names the listening process (lsof on macOS/Linux).
func portOwner(port int) string {
	out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return "unknown process"
	}
	var pid, cmd string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == "":
			pid = line[1:]
		case strings.HasPrefix(line, "c") && cmd == "":
			cmd = line[1:]
		}
	}
	if cmd == "" {
		return "unknown process"
	}
	return fmt.Sprintf("%s (pid %s)", cmd, pid)
}

var knownDevServers = []string{"nginx", "httpd", "caddy", "herd", "valet", "php-fpm", "mamp"}

// ChoosePorts returns the ports Envoy publishes and a human explanation of the choice.
func ChoosePorts() (httpPort, httpsPort int, notes []string) {
	var busy []portCheck
	for _, port := range []int{80, 443} {
		for _, host := range []string{"127.0.0.1", "::1"} {
			if pc := checkPort(host, port); pc.Busy {
				busy = append(busy, pc)
			}
		}
	}
	if len(busy) == 0 {
		return 80, 443, []string{"ports 80/443 are free: default mode"}
	}
	for _, pc := range busy {
		note := fmt.Sprintf("%s is taken by %s", pc.Addr, pc.Owner)
		for _, k := range knownDevServers {
			if strings.Contains(strings.ToLower(pc.Owner), k) {
				note += "; it is a known dev server: move it to 8080 to let Devcorb take 80/443 (coexistence mode)"
				break
			}
		}
		notes = append(notes, note)
	}
	notes = append(notes, fmt.Sprintf("fallback mode: URLs use ports %d/%d; the other service was not touched", fallbackHTTP, fallbackHTTPS))
	return fallbackHTTP, fallbackHTTPS, notes
}

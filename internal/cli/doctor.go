package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/pki"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/reconcile"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/trust"
)

// check is one preflight result.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, info
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (a *app) doctorCmd() *cobra.Command {
	var fix, yes bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the machine: container runtime, ports 80/443, *.localhost, the local CA",
		Long: "Checks what Devopus needs from this machine and says who is in the way and how to fix it.\n" +
			"--fix applies the fixes Devopus can make itself; each one asks first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			checks := a.preflight(ctx)
			if fix {
				var err error
				if checks, err = a.applyFixes(ctx, checks, yes); err != nil {
					return err
				}
			}
			failed := false
			for _, c := range checks {
				failed = failed || c.Status == "fail"
			}
			if a.json {
				if err := a.writeJSON(map[string]any{"checks": checks, "ok": !failed}); err != nil {
					return err
				}
			} else {
				a.printChecks(checks)
			}
			if failed {
				return silent(ExitPreflightErr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "apply the fixes Devopus can make (asks before each)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --fix: do not ask")
	return cmd
}

func (a *app) printChecks(checks []check) {
	marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "info": "·"}
	for _, c := range checks {
		fmt.Fprintf(a.stdout, "%s %-9s %s\n", marks[c.Status], c.Name, indent(c.Detail, 12))
		if c.Fix != "" {
			fmt.Fprintf(a.stdout, "            → %s\n", indent(c.Fix, 14))
		}
	}
}

func indent(s string, n int) string {
	return strings.ReplaceAll(s, "\n", "\n"+strings.Repeat(" ", n))
}

func (a *app) preflight(ctx context.Context) []check {
	var out []check
	ep := runtime.Detect()
	rtName := ep.Name
	rt, err := runtime.New(ep)
	if err == nil {
		var info runtime.Info
		if info, err = rt.Info(ctx); err == nil {
			rtName = info.Name
			out = append(out, check{Name: "runtime", Status: "ok", Detail: fmt.Sprintf("%s %s (%s)", info.Name, info.ServerVersion, ep.Host)})
		}
		rt.Close()
	}
	if err != nil {
		out = append(out, check{Name: "runtime", Status: "fail", Detail: fmt.Sprintf("%s at %s does not answer: %v", ep.Name, ep.Host, err),
			Fix: "start Docker Desktop, OrbStack ('orb start') or Colima ('colima start')"})
	}

	var status *agentv2.StatusResponse
	c := client.New(paths.Socket())
	if res, err := c.Status(ctx, connect.NewRequest(&agentv2.StatusRequest{})); err == nil {
		status = res.Msg
		out = append(out, check{Name: "agent", Status: "ok", Detail: fmt.Sprintf("running, version %s", status.AgentVersion)})
	} else {
		out = append(out, check{Name: "agent", Status: "info", Detail: "not running; 'devopus agent start' or any command that needs it starts it"})
	}

	out = append(out, check{Name: "panel", Status: "info", Detail: panelLine(ctx)})
	out = append(out, portChecks(ctx, status, rtName)...)
	out = append(out, dnsCheck(ctx))
	out = append(out, caCheck())
	out = append(out, poolCheck())
	return out
}

// portChecks looks at the proxy if it runs, otherwise at who holds 80/443.
func portChecks(ctx context.Context, status *agentv2.StatusResponse, rtName string) []check {
	var out []check
	p := status.GetProxy()
	if p.GetState() == "running" {
		ports := proxy.Ports{HTTP: uint16(p.HttpPort), HTTPS: uint16(p.HttpsPort)}
		var missing []string
		for _, ip := range proxy.Loopback {
			if !reachable(ctx, netip.AddrPortFrom(ip, ports.HTTPS)) {
				missing = append(missing, netip.AddrPortFrom(ip, ports.HTTPS).String())
			}
		}
		c := check{Name: "ports", Status: "ok", Detail: fmt.Sprintf("proxy on %d/%d of 127.0.0.1 and ::1 (%s mode)", ports.HTTP, ports.HTTPS, p.Mode)}
		if p.Mode == "coexist" {
			c.Detail += fmt.Sprintf("; other hosts go to the dev server on %d/%d", p.CoexistHttpPort, p.CoexistHttpsPort)
		}
		if p.Mode == "fallback" {
			c.Status = "warn"
			c.Detail = fmt.Sprintf("proxy on the fallback ports %d/%d: URLs carry :%d", ports.HTTP, ports.HTTPS, ports.HTTPS)
			for _, cf := range p.Conflicts {
				c.Detail += "\n" + conflictLine(cf.Address, cf.Owner, cf.Pid)
			}
			if len(p.Conflicts) > 0 {
				c.Fix = p.Conflicts[0].Hint
			} else {
				c.Fix = "80/443 look free now: 'devopus proxy restart' moves the proxy there (URLs change)"
			}
		}
		if len(missing) > 0 {
			c.Status, c.Detail = "fail", c.Detail+"\nnot answering on "+strings.Join(missing, ", ")
			c.Fix = "docker logs " + p.Container
		}
		out = append(out, c)
	} else {
		conflicts := proxy.Taken(ctx, proxy.DefaultPorts)
		if len(conflicts) == 0 {
			out = append(out, check{Name: "ports", Status: "ok", Detail: "80 and 443 are free on 127.0.0.1 and ::1"})
		} else {
			c := check{Name: "ports", Status: "warn", Detail: fmt.Sprintf("Devopus will use the fallback ports %d/%d:", proxy.FallbackPorts.HTTP, proxy.FallbackPorts.HTTPS)}
			for _, cf := range conflicts {
				c.Detail += "\n" + conflictLine(cf.Addr.String(), cf.Owner, int32(cf.PID))
			}
			c.Fix = conflicts[0].Hint()
			if more := proxy.Taken(ctx, proxy.FallbackPorts); len(more) > 0 {
				c.Status = "fail"
				c.Detail += "\nand the fallback ports are taken too: " + more[0].String()
				c.Fix = more[0].Hint()
			}
			out = append(out, c)
		}
		if p.GetState() == "error" {
			out = append(out, check{Name: "proxy", Status: "fail", Detail: p.Error})
		}
	}
	if rtName == "colima" || rtName == "orbstack" {
		out = append(out, check{Name: "exposure", Status: "warn",
			Detail: rtName + " publishes the proxy's ports on all interfaces, not only on loopback.\nIt does not forward traffic from other machines, but the port is open on the LAN interface\nand the macOS firewall may ask about it (P-6)."})
	}
	return out
}

func conflictLine(addr, owner string, pid int32) string {
	if owner == "" {
		return addr + ": taken by a program lsof cannot see (another user's)"
	}
	if pid > 0 {
		return fmt.Sprintf("%s: %s (pid %d)", addr, owner, pid)
	}
	return addr + ": " + owner
}

func reachable(ctx context.Context, ap netip.AddrPort) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ap.String())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// dnsCheck resolves *.localhost names the way most programs on the host do.
func dnsCheck(ctx context.Context) check {
	var bad []string
	for _, name := range []string{"devopus-doctor.localhost", "pay-123.devopus-doctor.localhost"} {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
		ok := err == nil && len(addrs) > 0
		for _, a := range addrs {
			ok = ok && a.Unmap().IsLoopback()
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s → %v %v", name, addrs, err))
		}
	}
	if len(bad) > 0 {
		return check{Name: "dns", Status: "fail", Detail: "*.localhost does not resolve to loopback:\n" + strings.Join(bad, "\n"),
			Fix: "a VPN or a custom resolver may override .localhost; macOS before 26 needs /etc/resolver/localhost, which Devopus does not set up yet"}
	}
	return check{Name: "dns", Status: "ok", Detail: "*.localhost resolves to loopback (nested names too)"}
}

func caCheck() check {
	ca, err := pki.LoadCA(paths.CA())
	if errors.Is(err, os.ErrNotExist) {
		return check{Name: "ca", Status: "info", Detail: "not created yet; the agent creates it on the first 'devopus up'"}
	}
	if err != nil {
		return check{Name: "ca", Status: "fail", Detail: err.Error()}
	}
	detail := fmt.Sprintf("%s\nSHA-256 %s", ca.RootPath(), ca.Fingerprint())
	ok, err := trust.Trusted(ca)
	switch {
	case err != nil:
		return check{Name: "ca", Status: "fail", Detail: detail + "\n" + err.Error()}
	case !ok:
		return check{Name: "ca", Status: "fail", Detail: "the local CA is not trusted: browsers warn on every environment\n" + detail,
			Fix: "devopus doctor --fix (adds it to your login keychain), or by hand: " + trust.ManualSteps(ca.RootPath())}
	}
	return check{Name: "ca", Status: "ok", Detail: "trusted by the system\n" + detail +
		"\nFirefox uses it when security.enterprise_roots.enabled is on (the default on macOS)"}
}

// poolCheck warns when a host interface (a VPN, typically) uses the environment
// address pool: those addresses would be unreachable from containers.
func poolCheck() check {
	ifaces, err := net.Interfaces()
	if err != nil {
		return check{Name: "addresses", Status: "info", Detail: err.Error()}
	}
	for _, ifc := range ifaces {
		if strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err == nil && p.Overlaps(reconcile.Pool) {
				return check{Name: "addresses", Status: "warn", Detail: fmt.Sprintf("%s has %s inside the environment pool %s", ifc.Name, p, reconcile.Pool),
					Fix: "environments skip subnets Docker reports as used, but a VPN route is invisible to it; tell us which VPN this is"}
			}
		}
	}
	return check{Name: "addresses", Status: "ok", Detail: fmt.Sprintf("the environment pool %s is free on this machine", reconcile.Pool)}
}

// applyFixes trusts the CA, the one fix Devopus makes itself in M3.
func (a *app) applyFixes(ctx context.Context, checks []check, yes bool) ([]check, error) {
	for i, c := range checks {
		if c.Name != "ca" || c.Status != "fail" || !trust.CanInstall() {
			continue
		}
		ca, err := pki.LoadOrCreateCA(paths.CA())
		if err != nil {
			return nil, fail(ExitError, "%v", err)
		}
		ok, err := a.confirm(yes, fmt.Sprintf("Add the Devopus local CA to your login keychain?\n  %s\n  SHA-256 %s\n  It can sign only names under .localhost. macOS will ask for your password.", ca.RootPath(), ca.Fingerprint()))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := trust.Install(ca.RootPath()); err != nil {
			return nil, fail(ExitError, "%v", err)
		}
		checks[i] = caCheck()
	}
	return checks, nil
}

// confirm asks a yes/no question; without a terminal it refuses with exit code 3.
func (a *app) confirm(yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, fail(ExitConfirm, "confirmation needed: run in a terminal or pass --yes")
	}
	fmt.Fprintf(a.stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

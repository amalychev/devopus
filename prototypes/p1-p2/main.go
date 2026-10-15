// Command devopus is the stage-0 prototype for P-1 (workspace.yaml → stack on *.localhost
// with HTTPS) and P-2 (Docker isolation between features). Throwaway code: see README.md.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `devopus (prototype P-1/P-2)

  devopus doctor                              check Docker, ports 80/443, *.localhost
  devopus validate [-f workspace.yaml]
  devopus up [-f workspace.yaml]              start the base env and every known feature
  devopus feature create <name> --modules a,b
  devopus feature rm <name> [--force]
  devopus list
  devopus routes
  devopus down [--purge] [--force]            stop everything; --purge also drops data and worktrees
`

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}
	log := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	flags, pos := parseFlags(args[1:])

	switch args[0] {
	case "doctor":
		return cmdDoctor(log)
	case "validate":
		_, err := LoadWorkspace(configFlag(flags))
		if err == nil {
			log("workspace is valid")
		}
		return err
	case "up":
		return cmdUp(configFlag(flags), log)
	case "down":
		return cmdDown(flags["purge"] != "", flags["force"] != "", log)
	case "list", "status":
		return cmdList(out)
	case "routes":
		return cmdRoutes(out)
	case "feature":
		if len(pos) < 2 {
			return errors.New("usage: devopus feature create|rm <name>")
		}
		switch pos[0] {
		case "create":
			mods := strings.Split(flags["modules"], ",")
			if flags["modules"] == "" {
				return errors.New("--modules is required, e.g. --modules frontend,payments")
			}
			return cmdFeatureCreate(pos[1], mods, log)
		case "rm":
			return cmdFeatureRm(pos[1], flags["force"] != "", log)
		}
	}
	return fmt.Errorf("unknown command %q\n\n%s", strings.Join(args, " "), usage)
}

func parseFlags(args []string) (map[string]string, []string) {
	flags := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f":
			if i+1 < len(args) {
				flags["file"] = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--"):
			k, v, ok := strings.Cut(a[2:], "=")
			if !ok {
				if k == "modules" && i+1 < len(args) {
					v = args[i+1]
					i++
				} else {
					v = "true"
				}
			}
			flags[k] = v
		default:
			pos = append(pos, a)
		}
	}
	return flags, pos
}

func configFlag(flags map[string]string) string {
	if f := flags["file"]; f != "" {
		return f
	}
	return "workspace.yaml"
}

func loadAll() (*State, *Workspace, error) {
	st, err := LoadState()
	if err != nil {
		return nil, nil, err
	}
	ws, err := LoadWorkspace(st.ConfigPath)
	return st, ws, err
}

func cmdDoctor(log func(string, ...any)) error {
	if v, err := docker("version", "--format", "{{.Server.Version}} ({{.Server.Os}})"); err != nil {
		log("✗ docker: %v", err)
	} else {
		log("✓ docker %s", v)
	}
	if v, err := git(".", "--version"); err == nil {
		log("✓ %s", v)
	}
	_, _, notes := ChoosePorts()
	for _, n := range notes {
		log("• %s", n)
	}
	if addrs, err := net.LookupHost("probe.devopus.localhost"); err != nil || len(addrs) == 0 {
		log("✗ *.localhost does not resolve through the system resolver: browsers work, curl/Node/Python may not (needs /etc/resolver/localhost + local DNS; not done by the prototype)")
	} else {
		log("✓ *.localhost resolves to %s", strings.Join(addrs, ", "))
	}
	log("• CA: %s (not installed in trust stores; pass it to clients explicitly)", CACertPath())
	return nil
}

func cmdUp(configPath string, log func(string, ...any)) error {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	ws, err := LoadWorkspace(abs)
	if err != nil {
		return err
	}
	st, err := LoadState()
	if errors.Is(err, errNoState) {
		st = &State{Project: ws.Project, Root: filepath.Dir(abs), ConfigPath: abs, NextNet: 1}
		var notes []string
		st.HTTPPort, st.HTTPSPort, notes = ChoosePorts()
		for _, n := range notes {
			log("ports: %s", n)
		}
	} else if err != nil {
		return err
	} else if st.ConfigPath != abs {
		return fmt.Errorf("the prototype manages one project; %s is already up (run `devopus down --purge` first)", st.ConfigPath)
	}

	base := st.Base()
	if base == nil {
		branch, err := currentBranch(st.Root)
		if err != nil {
			return fmt.Errorf("%s must be a git repository: %w", st.Root, err)
		}
		subnet, ip, err := st.AllocNet()
		if err != nil {
			return err
		}
		base = &Env{ID: newID(), Kind: KindBase, Branch: branch, Code: st.Root,
			Modules: ws.ModuleNames(), Services: ws.ServiceNames(), Subnet: subnet, EnvoyIP: ip}
		st.Envs = append([]*Env{base}, st.Envs...)
	}
	if err := st.Save(); err != nil {
		return err
	}

	log("building module images…")
	if err := BuildImages(st, ws); err != nil {
		return err
	}
	for _, env := range st.Envs {
		log("starting %s (%s)…", label(st, ws, env), env.ID)
		if err := composeUp(st, ws, env); err != nil {
			return err
		}
	}
	for _, env := range st.Envs {
		if err := runMigrations(st, ws, env, log); err != nil {
			return err
		}
	}
	if err := ApplyEnvoy(st, ws); err != nil {
		return err
	}
	for _, env := range st.Envs {
		if err := WaitReady(st, ws, env, 90*time.Second); err != nil {
			return err
		}
		log("✓ %s → %s", label(st, ws, env), st.URL(env, ws))
	}
	return nil
}

func cmdFeatureCreate(name string, forked []string, log func(string, ...any)) error {
	start := time.Now()
	st, ws, err := loadAll()
	if err != nil {
		return err
	}
	base := st.Base()
	if base == nil {
		return errNoState
	}
	if st.FindFeature(name) != nil {
		return fmt.Errorf("feature %q already exists", name)
	}
	modules, services, err := PlanFeature(ws, forked)
	if err != nil {
		return err
	}
	slug := st.uniqueSlug(name)
	id := newID()
	branch := "feature/" + slugify(name)
	wt := filepath.Join(homeDir(), "worktrees", st.Project, id)
	if err := addWorktree(st.Root, wt, branch, base.Branch); err != nil {
		return err
	}
	subnet, ip, err := st.AllocNet()
	if err != nil {
		return err
	}
	env := &Env{ID: id, Kind: KindFeature, Slug: slug, Branch: branch, Code: wt,
		Forked: forked, Modules: modules, Services: services, Subnet: subnet, EnvoyIP: ip}
	st.Envs = append(st.Envs, env)
	if err := st.Save(); err != nil {
		return err
	}

	sharedMods, sharedSvcs := SharedFrom(ws, env)
	log("feature %s (%s), branch %s", slug, id, branch)
	log("  worktree:        %s", wt)
	log("  own containers:  modules %s; services %s", list(modules), list(services))
	if extra := minus(modules, forked); len(extra) > 0 {
		log("  simple isolation also copies %s: they call a forked module", list(extra))
	}
	log("  shared with main: modules %s; services %s", list(sharedMods), list(sharedSvcs))
	for _, mod := range sharedMods {
		for _, u := range ws.Modules[mod].Uses {
			if contains(sharedSvcs, u) {
				log("  ⚠ %s writes to %s shared with main: fork it if this feature checks its data", mod, u)
				break
			}
		}
	}

	if err := composeUp(st, ws, env); err != nil {
		return err
	}
	if err := runMigrations(st, ws, env, log); err != nil {
		return err
	}
	if err := ApplyEnvoy(st, ws); err != nil {
		return err
	}
	if err := WaitReady(st, ws, env, 90*time.Second); err != nil {
		return err
	}
	log("✓ %s ready in %s", st.URL(env, ws), time.Since(start).Round(100*time.Millisecond))
	return nil
}

func cmdFeatureRm(name string, force bool, log func(string, ...any)) error {
	st, ws, err := loadAll()
	if err != nil {
		return err
	}
	env := st.FindFeature(name)
	if env == nil {
		return fmt.Errorf("no feature %q", name)
	}
	if err := checkSafeRemove(st, env, force); err != nil {
		return err
	}
	if err := composeDown(st, env, true); err != nil {
		return err
	}
	if err := removeWorktree(st.Root, env.Code); err != nil {
		return err
	}
	st.Remove(env)
	if err := st.Save(); err != nil {
		return err
	}
	if err := ApplyEnvoy(st, ws); err != nil {
		return err
	}
	log("removed feature %s; branch %s is kept", env.Slug, env.Branch)
	return nil
}

func checkSafeRemove(st *State, env *Env, force bool) error {
	problems, err := unsafeToRemove(env.Code, st.Base().Branch)
	if err != nil {
		return err
	}
	if len(problems) > 0 && !force {
		return fmt.Errorf("feature %s has %s in %s; commit/push it or pass --force", env.Slug, strings.Join(problems, " and "), env.Code)
	}
	return nil
}

func cmdDown(purge, force bool, log func(string, ...any)) error {
	st, err := LoadState()
	if err != nil {
		return err
	}
	if purge && !force {
		for _, env := range st.Envs {
			if env.Kind == KindFeature {
				if err := checkSafeRemove(st, env, false); err != nil {
					return err
				}
			}
		}
	}
	for _, env := range st.Envs {
		if err := composeDown(st, env, purge); err != nil {
			return err
		}
		if purge && env.Kind == KindFeature {
			if err := removeWorktree(st.Root, env.Code); err != nil {
				log("warning: %v", err)
			}
		}
	}
	RemoveEnvoy()
	if purge {
		log("purged %s: data volumes, worktrees and state removed", st.Project)
		return os.Remove(statePath())
	}
	log("stopped %s; `devopus up` brings back the base env and %d feature(s)", st.Project, len(st.Envs)-1)
	return nil
}

func cmdList(out io.Writer) error {
	st, ws, err := loadAll()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ENV\tID\tBRANCH\tOWN MODULES\tOWN SERVICES\tURL")
	for _, env := range st.Envs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", label(st, ws, env), env.ID, env.Branch, list(env.Modules), list(env.Services), st.URL(env, ws))
	}
	return tw.Flush()
}

func cmdRoutes(out io.Writer) error {
	st, ws, err := loadAll()
	if err != nil {
		return err
	}
	base := st.Base()
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tPATH\tMODULE\tSERVED BY")
	for _, env := range st.Envs {
		for _, name := range ws.ModuleNames() {
			owner := ownerOf(base, env, name, false)
			for _, r := range ws.Modules[name].Routes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", st.Host(env, ws), r, name, label(st, ws, owner))
			}
		}
	}
	return tw.Flush()
}

// HTTPClient talks to Envoy on loopback for any *.localhost host and trusts only the local CA.
func HTTPClient(st *State) (*http.Client, error) {
	pem, err := os.ReadFile(CACertPath())
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, _ := net.SplitHostPort(addr)
				if strings.HasSuffix(host, ".localhost") || host == "localhost" {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}, nil
}

// WaitReady polls every routed prefix of the env until none answers 5xx.
func WaitReady(st *State, ws *Workspace, env *Env, timeout time.Duration) error {
	client, err := HTTPClient(st)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ok := true
		for _, name := range ws.ModuleNames() {
			for _, r := range ws.Modules[name].Routes {
				resp, err := client.Get(st.URL(env, ws) + r)
				if err != nil {
					ok, last = false, err.Error()
					break
				}
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					ok, last = false, fmt.Sprintf("%s%s → %d", st.URL(env, ws), r, resp.StatusCode)
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s is not ready after %s: %s\nenvoy logs:\n%s", st.URL(env, ws), timeout, last, envoyLogsTail())
}

func label(st *State, ws *Workspace, env *Env) string {
	if env.Kind == KindBase {
		return "base (" + env.Branch + ")"
	}
	return env.Slug
}

func list(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}

func minus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

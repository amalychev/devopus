package compose

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/docklet/docklet/internal/workspace"
)

var (
	secretKey    = regexp.MustCompile(`(?i)(SECRET|PASSWORD|PASSWD|TOKEN|API_?KEY|PRIVATE|CREDENTIAL|_KEY$|^KEY_)`)
	frontendName = regexp.MustCompile(`(?i)front|web|ui|app|nginx|proxy|client|vote`)
	httpPorts    = map[int]bool{80: true, 443: true, 3000: true, 3001: true, 4000: true, 4200: true, 5000: true, 5173: true, 8000: true, 8025: true, 8080: true, 8081: true, 8888: true, 9000: true}
)

func convert(ctx context.Context, active, all *types.Project, root, name string, opt Options, rep *Report) *Result {
	slug := projectSlug(name)
	w := &workspace.Workspace{
		Version:  workspace.Version,
		Project:  slug,
		Domain:   slug + ".localhost",
		Services: map[string]*workspace.Service{},
		Modules:  map[string]*workspace.Module{},
	}
	res := &Result{Workspace: w, Secrets: map[string]string{}, Report: rep}

	// Logical names are DNS labels; compose allows underscores and capitals.
	rename := map[string]string{}
	for n := range all.Services {
		s := slugName(n)
		if s != n {
			rep.Renamed = append(rep.Renamed, n+" → "+s)
		}
		rename[n] = s
	}
	sort.Strings(rep.Renamed)

	var entries []entry
	for _, n := range sortedKeys(all.Services) {
		svc := all.Services[n]
		name := rename[n]
		_, isActive := active.Services[n]
		if a, ok := active.Services[n]; ok {
			svc = a // the active model has the default profile's view of the service
		}
		var uses []string
		for dep := range svc.DependsOn {
			uses = append(uses, rename[dep])
		}
		sort.Strings(uses)
		env := moveSecrets(res, name, environment(svc, root, rep))
		port, published := firstPort(svc)
		rep.DroppedPorts += countPublished(svc)
		unsupported(rep, name, svc)

		if svc.Build != nil {
			ctx := rel(root, svc.Build.Context)
			m := &workspace.Module{
				Path:        ctx,
				Port:        port,
				Uses:        uses,
				Env:         env,
				Command:     shell(svc.Command),
				Entrypoint:  shell(svc.Entrypoint),
				Healthcheck: healthcheck(svc.HealthCheck),
				Tmpfs:       svc.Tmpfs,
				Ulimits:     ulimits(svc.Ulimits),
				Optional:    !isActive,
				Build:       &workspace.Build{Context: ctx, Dockerfile: svc.Build.Dockerfile, Target: svc.Build.Target},
			}
			if strings.HasPrefix(ctx, "..") {
				rep.Errors = append(rep.Errors, name+": build context "+ctx+" is outside the repository; run docklet init from the repository root")
			}
			if m.Build.Dockerfile == "Dockerfile" {
				m.Build.Dockerfile = ""
			}
			for k, v := range svc.Build.Args {
				if v != nil {
					if m.Build.Args == nil {
						m.Build.Args = map[string]string{}
					}
					m.Build.Args[k] = *v
				}
			}
			m.Volumes = mounts(rep, root, name, svc, svc.Build.Context)
			if !isActive {
				rep.Optional = append(rep.Optional, name+" ("+strings.Join(svc.Profiles, ", ")+")")
			}
			if port == 0 {
				rep.AssumedPort = append(rep.AssumedPort, name)
			}
			w.Modules[name] = m
			if published {
				entries = append(entries, entry{name, port, true})
			}
			continue
		}

		if svc.Image == "" {
			if !isActive {
				// a template for extends behind a profile, never started on its own
				rep.Skipped = append(rep.Skipped, name)
				continue
			}
			rep.Errors = append(rep.Errors, name+": has neither image nor build")
			continue
		}
		if !isActive {
			rep.Optional = append(rep.Optional, name+" ("+strings.Join(svc.Profiles, ", ")+")")
		}
		image := pin(ctx, rep, opt.Pinner, name, svc.Image)
		s := &workspace.Service{
			Image:       image,
			Port:        port,
			Env:         env,
			Command:     shell(svc.Command),
			Entrypoint:  shell(svc.Entrypoint),
			Healthcheck: healthcheck(svc.HealthCheck),
			Tmpfs:       svc.Tmpfs,
			Ulimits:     ulimits(svc.Ulimits),
			Optional:    !isActive,
			Volumes:     mounts(rep, root, name, svc, ""),
		}
		w.Services[name] = s
		if published {
			entries = append(entries, entry{name, port, false})
		}
	}

	// A service cannot use a module in v1 (nginx in front of php-fpm): the proxy of the
	// environment does that job, so the edge is dropped and reported.
	for _, n := range sortedKeys(all.Services) {
		name := rename[n]
		s, ok := w.Services[name]
		if !ok {
			continue
		}
		deps := all.Services[n].DependsOn
		for dep := range deps {
			d := rename[dep]
			if _, isModule := w.Modules[d]; isModule {
				rep.ServiceOnModule = append(rep.ServiceOnModule, name+" → "+d)
				continue
			}
			s.Uses = append(s.Uses, d)
		}
		sort.Strings(s.Uses)
	}
	sort.Strings(rep.ServiceOnModule)

	assignEntryPoints(w, rep, entries)
	rep.Modules, rep.Services = len(w.Modules), len(w.Services)
	rep.NoCode = len(w.Modules) == 0
	for _, u := range rep.Unpinned {
		rep.Errors = append(rep.Errors, "image without a version: "+u)
	}
	if len(res.Secrets) > 0 {
		rep.Secrets = sortedKeys(res.Secrets)
	}
	return res
}

// moveSecrets replaces values of secret-looking keys with ${{ secrets.NAME }}.
func moveSecrets(res *Result, owner string, env map[string]*string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, k := range sortedKeys(env) {
		val := ""
		if v := env[k]; v != nil {
			val = *v
		}
		if secretKey.MatchString(k) && val != "" && !strings.HasPrefix(val, "${") {
			name := strings.ToUpper(k)
			if prev, taken := res.Secrets[name]; taken && prev != val {
				name = strings.ToUpper(strings.ReplaceAll(owner, "-", "_")) + "_" + name
			}
			res.Secrets[name] = val
			val = "${{ secrets." + name + " }}"
		}
		out[k] = val
	}
	return out
}

type entry struct {
	name     string
	port     int
	isModule bool
}

// assignEntryPoints turns published HTTP ports into routes. One entry point gets "/";
// with several, a frontend-looking module gets "/", other modules a path, services a subdomain.
func assignEntryPoints(w *workspace.Workspace, rep *Report, entries []entry) {
	var http []entry
	for _, e := range entries {
		if httpPorts[e.port] || e.port == 0 {
			http = append(http, e)
		}
	}
	root := -1
	for i, e := range http {
		if e.isModule && frontendName.MatchString(e.name) {
			root = i
			break
		}
	}
	if root < 0 {
		for i, e := range http {
			if e.isModule {
				root = i
				break
			}
		}
	}
	for i, e := range http {
		if !e.isModule {
			s := w.Services[e.name]
			s.Expose = e.name
			s.UIPort = e.port
			if s.Port == e.port {
				s.UIPort = 0
			}
			rep.Routes = append(rep.Routes, e.name+" → "+e.name+"."+w.Domain)
			continue
		}
		path := "/"
		if i != root {
			path = "/" + e.name
		}
		w.Modules[e.name].Routes = []string{path}
		rep.Routes = append(rep.Routes, e.name+" → "+w.Domain+path)
	}
}

func mounts(rep *Report, root, name string, svc types.ServiceConfig, buildCtx string) []string {
	var out []string
	for _, v := range svc.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			if v.Source != "" {
				out = append(out, v.Source+":"+v.Target)
			}
		case types.VolumeTypeBind:
			src := v.Source
			switch {
			case buildCtx != "" && within(buildCtx, src):
				rep.CodeMounts++ // the module's own code: the worktree provides it
			case within(root, src) && buildCtx == "":
				r := rel(root, src)
				if !strings.HasPrefix(r, ".") {
					r = "./" + r
				}
				out = append(out, r+":"+v.Target)
				rep.RepoMounts = append(rep.RepoMounts, name+": "+r+" → "+v.Target)
			case within(root, src):
				rep.RepoMounts = append(rep.RepoMounts, name+": "+rel(root, src)+" → "+v.Target+" (not kept: modules get code from the worktree)")
			default:
				rep.HostMounts = append(rep.HostMounts, name+": "+src+" → "+v.Target)
			}
		}
	}
	return out
}

func unsupported(rep *Report, name string, svc types.ServiceConfig) {
	add := func(feature string) { rep.Unsupported[feature] = appendUnique(rep.Unsupported[feature], name) }
	if svc.NetworkMode != "" {
		add("network_mode")
	}
	if svc.Privileged {
		add("privileged")
	}
	if len(svc.CapAdd) > 0 {
		add("cap_add")
	}
	if len(svc.Devices) > 0 {
		add("devices")
	}
	if len(svc.Secrets) > 0 {
		add("secrets (files)")
	}
	if len(svc.Configs) > 0 {
		add("configs")
	}
	if len(svc.Sysctls) > 0 {
		add("sysctls")
	}
	if svc.Deploy != nil && svc.Deploy.Resources.Limits != nil {
		add("deploy.resources")
	}
	if len(svc.ExtraHosts) > 0 {
		add("extra_hosts")
	}
	if svc.User != "" {
		add("user")
	}
	if svc.WorkingDir != "" && svc.Build == nil {
		add("working_dir")
	}
	for n := range svc.Networks {
		if n != "default" {
			add("networks (one network per environment instead)")
		}
	}
}

func healthcheck(h *types.HealthCheckConfig) *workspace.Healthcheck {
	if h == nil {
		return nil
	}
	out := &workspace.Healthcheck{Test: workspace.Command(h.Test), Disable: h.Disable}
	dur := func(d *types.Duration) string {
		if d == nil {
			return ""
		}
		return compactDuration(time.Duration(*d))
	}
	out.Interval, out.Timeout, out.StartPeriod = dur(h.Interval), dur(h.Timeout), dur(h.StartPeriod)
	if h.Retries != nil {
		out.Retries = int(*h.Retries)
	}
	return out
}

// compactDuration prints 1m30s rather than Go's 1m30s0ms forms and drops zero units.
func compactDuration(d time.Duration) string {
	if d%time.Second != 0 {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	s := d.String()
	s = strings.Replace(s, "m0s", "m", 1)
	s = strings.Replace(s, "h0m", "h", 1)
	return s
}

func ulimits(u map[string]*types.UlimitsConfig) map[string]*workspace.Ulimit {
	if len(u) == 0 {
		return nil
	}
	out := map[string]*workspace.Ulimit{}
	for k, v := range u {
		out[k] = &workspace.Ulimit{Single: v.Single, Soft: v.Soft, Hard: v.Hard}
	}
	return out
}

func firstPort(svc types.ServiceConfig) (int, bool) {
	for _, p := range svc.Ports {
		return int(p.Target), p.Published != ""
	}
	for _, e := range svc.Expose {
		if v, err := strconv.Atoi(strings.Split(e, "/")[0]); err == nil {
			return v, false
		}
	}
	return 0, false
}

func countPublished(svc types.ServiceConfig) int {
	n := 0
	for _, p := range svc.Ports {
		if p.Published != "" {
			n++
		}
	}
	return n
}

// shell joins a command into one string that splits back into the same arguments.
func shell(c types.ShellCommand) string {
	parts := make([]string, len(c))
	for i, a := range c {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>(){}*?!#~") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(parts, " ")
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func within(dir, p string) bool {
	r, err := filepath.Rel(dir, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugName(s string) string {
	s = strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "s-" + s
	}
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// projectSlug fits ^[a-z][a-z0-9-]{1,30}$.
func projectSlug(s string) string {
	s = slugName(s)
	if len(s) > 31 {
		s = strings.TrimRight(s[:31], "-")
	}
	if len(s) < 2 {
		s = fmt.Sprintf("%s-app", s)
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

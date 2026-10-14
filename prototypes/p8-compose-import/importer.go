package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// Draft is workspace.yaml v1 (specs/workspace-yaml.md) as produced by the importer.
// Build is an object here: compose projects need context + dockerfile + target + args,
// which the v1 spec's `build: <dir>` cannot express (finding of P-8).
type Draft struct {
	Version  int                      `yaml:"version"`
	Project  string                   `yaml:"project"`
	Domain   string                   `yaml:"domain"`
	Services map[string]*DraftService `yaml:"services,omitempty"`
	Modules  map[string]*DraftModule  `yaml:"modules"`
}

type DraftService struct {
	Image   string            `yaml:"image"`
	Port    int               `yaml:"port,omitempty"`
	Expose  string            `yaml:"expose,omitempty"`
	Volume  string            `yaml:"volume,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Command string            `yaml:"command,omitempty"`
	Uses    []string          `yaml:"uses,omitempty"`
}

type DraftModule struct {
	Path    string            `yaml:"path"`
	Build   *DraftBuild       `yaml:"build,omitempty"`
	Image   string            `yaml:"image,omitempty"`
	Port    int               `yaml:"port,omitempty"`
	Routes  []string          `yaml:"routes,omitempty"`
	Uses    []string          `yaml:"uses,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Command string            `yaml:"command,omitempty"`
	Volumes []string          `yaml:"volumes,omitempty"`
}

type DraftBuild struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Target     string            `yaml:"target,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
}

// Report lists what the importer did and what a human still has to look at.
type Report struct {
	Name         string
	Modules      int
	Services     int
	InProfiles   []string // imported, but optional in compose
	DroppedPorts int
	Routes       []string // guessed entry points: "name → /path"
	Secrets      []string // env keys that look like secrets
	CodeMounts   int      // bind mounts of module code, replaced by the worktree
	RepoMounts   []string // bind mounts of other repo files, kept
	HostMounts   []string // bind mounts outside the repo: need local confirmation
	Unsupported  map[string][]string
	Unpinned     []string
	HostVars     []string // variables the compose file expects from the host env
	Renamed      []string
	Errors       []string // blocking
	Pinned       []string // images pinned by digest automatically
	PinLater     []string // registry rate-limited the lookup: pin on the next attempt
	NoCode       bool     // only ready-made images: nothing to develop or fork
	LoadError    string
	RoundTrip    string
}

func (r *Report) Level() string {
	switch {
	case r.NoCode && r.LoadError == "":
		return "N"
	case r.LoadError != "" || len(r.Errors) > 0:
		return "C"
	case len(r.Pinned) > 0 || len(r.PinLater) > 0 || len(r.Routes) > 1 || len(r.Secrets) > 0 || len(r.HostMounts) > 0 || len(r.Unsupported) > 0 || len(r.InProfiles) > 0 || len(r.HostVars) > 0:
		return "B"
	default:
		return "A"
	}
}

var (
	envFileMissing = regexp.MustCompile(`env file (\S+) not found`)
	requiredVar    = regexp.MustCompile(`required variable (\S+) is missing a value`)
	nameRe         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`) // a DNS label; v1 spec said 31, too strict (Sentry)
	secretKey      = regexp.MustCompile(`(?i)(SECRET|PASSWORD|PASSWD|TOKEN|API_?KEY|PRIVATE|CREDENTIAL|_KEY$|^KEY_)`)
	httpPorts      = map[uint32]bool{80: true, 443: true, 3000: true, 3001: true, 4000: true, 4200: true, 5000: true, 5173: true, 8000: true, 8080: true, 8081: true, 8888: true, 9000: true}
)

// Import loads one compose file (plus siblings for extends) laid out under root and maps it.
func Import(name, root, file string) (*Draft, *Report) {
	rep := &Report{Name: name, Unsupported: map[string][]string{}}
	env := map[string]string{}
	// docker compose reads the project's .env for interpolation; the loader alone does not.
	if dot, err := dotenv.GetEnvFromFile(nil, []string{filepath.Join(filepath.Dir(file), ".env")}); err == nil {
		for k, v := range dot {
			env[k] = v
		}
	}
	var project *types.Project
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		project, err = loader.LoadWithContext(context.Background(), types.ConfigDetails{
			WorkingDir:  filepath.Dir(file),
			ConfigFiles: []types.ConfigFile{{Filename: file}},
			Environment: env,
		}, func(o *loader.Options) {
			o.SetProjectName(name, true)
			o.ResolvePaths = true // default profiles: what `docker compose up` starts
		})
		if err == nil {
			break
		}
		// Repositories ship .env examples, not .env files; the importer does not need values.
		if m := envFileMissing.FindStringSubmatch(err.Error()); m != nil {
			p := strings.Trim(m[1], `"'`)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, nil, 0o644)
			continue
		}
		if m := requiredVar.FindStringSubmatch(err.Error()); m != nil {
			env[m[1]] = "${" + m[1] + "}"
			rep.HostVars = appendUnique(rep.HostVars, m[1])
			continue
		}
		rep.LoadError = firstLine(err.Error())
		return nil, rep
	}
	if err != nil {
		rep.LoadError = firstLine(err.Error())
		return nil, rep
	}

	// Services behind non-default profiles are optional in compose; list them for review.
	if all, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir: filepath.Dir(file), ConfigFiles: []types.ConfigFile{{Filename: file}}, Environment: env,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.Profiles = []string{"*"}
		o.SkipConsistencyCheck = true
	}); err == nil {
		for n, svc := range all.Services {
			if _, active := project.Services[n]; !active && len(svc.Profiles) > 0 {
				rep.InProfiles = append(rep.InProfiles, n+" ("+strings.Join(svc.Profiles, ",")+")")
			}
		}
		sort.Strings(rep.InProfiles)
	}

	d := &Draft{Version: 1, Project: slug(name), Domain: slug(name) + ".localhost",
		Services: map[string]*DraftService{}, Modules: map[string]*DraftModule{}}

	// Names: our logical names are DNS-like; compose allows underscores and capitals.
	rename := map[string]string{}
	for n := range project.Services {
		s := slug(n)
		if s != n {
			rep.Renamed = append(rep.Renamed, n+" → "+s)
		}
		rename[n] = s
	}

	var entries []entry
	for _, n := range sortedNames(project.Services) {
		svc := project.Services[n]
		name := rename[n]
		uses := []string{}
		for dep := range svc.DependsOn {
			uses = append(uses, rename[dep])
		}
		sort.Strings(uses)
		envMap := map[string]string{}
		for k, v := range svc.Environment {
			val := ""
			if v != nil {
				val = *v
			}
			if secretKey.MatchString(k) && val != "" {
				rep.Secrets = append(rep.Secrets, name+"."+k)
				val = "${{ secrets." + strings.ToUpper(k) + " }}"
			}
			envMap[k] = val
		}
		port, published := firstPort(svc)
		rep.DroppedPorts += countPublished(svc)
		unsupported(rep, name, svc)

		if svc.Build != nil {
			ctx := rel(root, svc.Build.Context)
			m := &DraftModule{Path: ctx, Port: port, Uses: uses, Env: envMap, Command: shell(svc.Command)}
			m.Build = &DraftBuild{Context: ctx, Dockerfile: svc.Build.Dockerfile, Target: svc.Build.Target}
			if m.Build.Dockerfile == "Dockerfile" {
				m.Build.Dockerfile = ""
			}
			for k, v := range svc.Build.Args {
				if m.Build.Args == nil {
					m.Build.Args = map[string]string{}
				}
				if v != nil {
					m.Build.Args[k] = *v
				}
			}
			m.Volumes = mounts(rep, root, svc, svc.Build.Context)
			d.Modules[name] = m
			if published {
				entries = append(entries, entry{name, port, true})
			}
		} else {
			if svc.Image == "" {
				rep.Errors = append(rep.Errors, name+": ни image, ни build")
				continue
			}
			if !pinned(svc.Image) {
				// Same image for everyone: pin today's digest of the tag, or block if unknown.
				switch digest, limited := resolveDigest(svc.Image); {
				case digest != "":
					rep.Pinned = append(rep.Pinned, name+": "+svc.Image+" → @"+digest[:19]+"…")
					svc.Image = strings.SplitN(svc.Image, "@", 2)[0] + "@" + digest
				case limited:
					rep.PinLater = append(rep.PinLater, name+": "+svc.Image)
				default:
					rep.Unpinned = append(rep.Unpinned, name+": "+svc.Image)
				}
			}
			s := &DraftService{Image: svc.Image, Port: port, Env: envMap, Command: shell(svc.Command), Uses: uses}
			for _, v := range svc.Volumes {
				if v.Type == types.VolumeTypeVolume && s.Volume == "" {
					s.Volume = v.Target
				}
			}
			_ = mounts(rep, root, svc, "")
			d.Services[name] = s
			if published {
				entries = append(entries, entry{name, port, false})
			}
		}
	}
	assignEntryPoints(d, rep, entries)
	rep.Modules, rep.Services = len(d.Modules), len(d.Services)

	if len(d.Modules) == 0 {
		// Only ready-made images: a way to run software, not a product to develop.
		rep.NoCode = true
	}
	for _, u := range rep.Unpinned {
		rep.Errors = append(rep.Errors, "образ без версии — "+u)
	}
	validate(d, rep)
	return d, rep
}

type entry struct {
	name     string
	port     int
	isModule bool
}

// assignEntryPoints turns published HTTP ports into routes. One entry point gets "/";
// with several, a frontend-looking one gets "/" and the others a path or a subdomain.
func assignEntryPoints(d *Draft, rep *Report, entries []entry) {
	var http []entry
	for _, e := range entries {
		if httpPorts[uint32(e.port)] || e.port == 0 {
			http = append(http, e)
		}
	}
	if len(http) == 0 {
		return
	}
	root := 0
	for i, e := range http {
		if regexp.MustCompile(`(?i)front|web|ui|app|nginx|proxy|client|vote`).MatchString(e.name) {
			root = i
			break
		}
	}
	for i, e := range http {
		path := "/"
		if i != root {
			path = "/" + e.name
		}
		if e.isModule {
			d.Modules[e.name].Routes = []string{path}
			rep.Routes = append(rep.Routes, e.name+" → "+path)
		} else {
			d.Services[e.name].Expose = e.name
			rep.Routes = append(rep.Routes, e.name+" → "+e.name+".<host> (сервис)")
		}
	}
}

func mounts(rep *Report, root string, svc types.ServiceConfig, buildCtx string) []string {
	var named []string
	for _, v := range svc.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			if v.Source != "" {
				named = append(named, v.Source+":"+v.Target)
			}
		case types.VolumeTypeBind:
			src := v.Source
			switch {
			case buildCtx != "" && within(buildCtx, src):
				rep.CodeMounts++ // the module's own code: the worktree provides it
			case within(root, src):
				rep.RepoMounts = append(rep.RepoMounts, rel(root, src)+" → "+v.Target)
			default:
				rep.HostMounts = append(rep.HostMounts, src+" → "+v.Target)
			}
		}
	}
	return named
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
		add("secrets (файлы)")
	}
	if len(svc.Configs) > 0 {
		add("configs")
	}
	if len(svc.Sysctls) > 0 {
		add("sysctls")
	}
	if len(svc.Ulimits) > 0 {
		add("ulimits")
	}
	if svc.Deploy != nil && svc.Deploy.Resources.Limits != nil {
		add("deploy.resources")
	}
	if len(svc.Tmpfs) > 0 {
		add("tmpfs")
	}
	if len(svc.Entrypoint) > 0 {
		add("entrypoint")
	}
	if svc.HealthCheck != nil && svc.Build != nil {
		add("healthcheck у модуля")
	}
	for n := range svc.Networks {
		if n != "default" {
			add("networks (свои сети)")
		}
	}
}

func validate(d *Draft, rep *Report) {
	names := map[string]bool{}
	for n := range d.Modules {
		names[n] = true
	}
	for n := range d.Services {
		names[n] = true
	}
	routes := map[string]string{}
	for n, m := range d.Modules {
		if !nameRe.MatchString(n) {
			rep.Errors = append(rep.Errors, "имя модуля "+n+" не подходит под "+nameRe.String())
		}
		for _, u := range m.Uses {
			if !names[u] {
				rep.Errors = append(rep.Errors, n+".uses: неизвестный "+u)
			}
		}
		for _, r := range m.Routes {
			if o, ok := routes[r]; ok {
				rep.Errors = append(rep.Errors, "маршрут "+r+" у "+n+" и "+o)
			}
			routes[r] = n
		}
		if strings.HasPrefix(m.Path, "..") {
			rep.Errors = append(rep.Errors, n+".path вне репозитория: "+m.Path)
		}
	}
	for n := range d.Services {
		if !nameRe.MatchString(n) {
			rep.Errors = append(rep.Errors, "имя сервиса "+n+" не подходит под "+nameRe.String())
		}
	}
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

func pinned(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return true
	}
	last := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return false
	}
	tag := last[i+1:]
	return tag != "latest" && !strings.Contains(tag, "${")
}

// shell joins a command back into one string that splits into the same arguments.
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
	if r == "." {
		return "."
	}
	return filepath.ToSlash(r)
}

func within(dir, p string) bool {
	r, err := filepath.Rel(dir, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}

func slug(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func sortedNames(m types.Services) []string {
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// resolveDigest pins a tag to today's digest. Registries rate-limit anonymous lookups
// (Docker Hub refused some on reruns), so results are cached and failures retried once.
func resolveDigest(image string) (digest string, rateLimited bool) {
	cache := map[string]string{}
	const cacheFile = ".corpus/digests.json"
	if data, err := os.ReadFile(cacheFile); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	if d, ok := cache[image]; ok {
		return d, false
	}
	for attempt := 0; attempt < 2; attempt++ {
		cmd := exec.Command("docker", "buildx", "imagetools", "inspect", "--format", "{{json .Manifest.Digest}}", image)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		rateLimited = strings.Contains(stderr.String(), "429")
		if err == nil {
			d := strings.Trim(strings.TrimSpace(string(out)), `"`)
			cache[image] = d
			data, _ := json.MarshalIndent(cache, "", "  ")
			_ = os.WriteFile(cacheFile, data, 0o644)
			return d, false
		}
		time.Sleep(3 * time.Second)
	}
	return "", rateLimited
}

var _ = fmt.Sprint

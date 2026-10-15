// Command p8import is the stage-0 prototype P-8: how much of real docker-compose projects
// can be turned into workspace.yaml v1 without manual edits? It downloads a public corpus,
// imports each file, validates the draft, exports it back to compose and checks that with
// `docker compose config`. Throwaway code: see README.md.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type source struct {
	Name, Group string
	URL         string
	Dir, File   string            // where the file sits in its repository
	Extra       map[string]string // sibling files needed for extends: name → URL
}

const raw = "https://raw.githubusercontent.com/"

var corpus = []source{
	{Name: "example-voting-app", Group: "product", URL: raw + "dockersamples/example-voting-app/main/docker-compose.yml", File: "docker-compose.yml"},
	// The root docker-compose.yml of Mastodon runs released images (no build); its dev stack is the devcontainer.
	{Name: "mastodon-dev", Group: "product", URL: raw + "mastodon/mastodon/main/.devcontainer/compose.yaml", Dir: ".devcontainer", File: "compose.yaml"},
	{Name: "sentry-self-hosted", Group: "product", URL: raw + "getsentry/self-hosted/master/docker-compose.yml", File: "docker-compose.yml",
		Extra: map[string]string{".env": raw + "getsentry/self-hosted/master/.env"}},
	{Name: "immich-dev", Group: "product", URL: raw + "immich-app/immich/main/docker/docker-compose.dev.yml", Dir: "docker", File: "docker-compose.dev.yml"},
	{Name: "posthog-dev", Group: "product", URL: raw + "PostHog/posthog/master/docker-compose.dev.yml", File: "docker-compose.dev.yml",
		Extra: map[string]string{"docker-compose.base.yml": raw + "PostHog/posthog/master/docker-compose.base.yml"}},
}

func init() {
	for _, s := range []string{"react-express-mongodb", "nginx-golang-mysql", "django", "flask-redis", "spring-postgres",
		"react-java-mysql", "nextcloud-redis-mariadb", "wordpress-mysql", "angular", "fastapi", "nginx-flask-mysql",
		"react-rust-postgres", "vuejs", "traefik-golang", "elasticsearch-logstash-kibana"} {
		corpus = append(corpus, source{Name: s, Group: "sample", URL: raw + "docker/awesome-compose/master/" + s + "/compose.yaml", File: "compose.yaml"})
	}
}

func main() {
	cache := ".corpus"
	work, err := os.MkdirTemp("", "p8-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(work)
	_ = os.MkdirAll(filepath.Join(cache, "drafts"), 0o755)

	var reports []*Report
	for _, src := range corpus {
		root := filepath.Join(work, src.Name, "repo")
		dir := filepath.Join(root, src.Dir)
		_ = os.MkdirAll(dir, 0o755)
		file := filepath.Join(dir, src.File)
		if err := fetch(src.URL, filepath.Join(cache, src.Name+"--"+src.File), file); err != nil {
			reports = append(reports, &Report{Name: src.Name, LoadError: "не скачалось: " + err.Error()})
			continue
		}
		for n, u := range src.Extra {
			_ = fetch(u, filepath.Join(cache, src.Name+"--"+n), filepath.Join(dir, n))
		}
		d, rep := Import(src.Name, root, file)
		if d != nil {
			out, _ := yaml.Marshal(d)
			_ = os.WriteFile(filepath.Join(cache, "drafts", src.Name+".workspace.yaml"), out, 0o644)
			rep.RoundTrip = roundTrip(d, root)
		}
		reports = append(reports, rep)
		fmt.Fprintf(os.Stderr, "%-30s уровень %s  модулей %d, сервисов %d %s\n", src.Name, rep.Level(), rep.Modules, rep.Services, rep.LoadError)
	}
	fmt.Print(render(reports))
}

// fetch keeps a local copy so reruns do not depend on the network.
func fetch(url, cached, dst string) error {
	data, err := os.ReadFile(cached)
	if err != nil {
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s: %s", url, resp.Status)
		}
		data, _ = io.ReadAll(resp.Body)
		_ = os.WriteFile(cached, data, 0o644)
	}
	return os.WriteFile(dst, data, 0o644)
}

// roundTrip exports the draft back to compose (base environment, no Devopus routing) and
// lets Docker Compose validate it: the "no lock-in" promise of specs/workspace-yaml.md.
func roundTrip(d *Draft, root string) string {
	type svc struct {
		Image       string            `yaml:"image,omitempty"`
		Build       map[string]any    `yaml:"build,omitempty"`
		Command     string            `yaml:"command,omitempty"`
		Environment map[string]string `yaml:"environment,omitempty"`
		DependsOn   []string          `yaml:"depends_on,omitempty"`
		Volumes     []string          `yaml:"volumes,omitempty"`
	}
	out := struct {
		Services map[string]svc      `yaml:"services"`
		Volumes  map[string]struct{} `yaml:"volumes,omitempty"`
	}{Services: map[string]svc{}, Volumes: map[string]struct{}{}}
	plain := func(env map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range env {
			// Secrets are filled by the agent; in an exported file they become host variables.
			v = strings.ReplaceAll(v, "${{ secrets.", "${")
			m[k] = strings.ReplaceAll(v, " }}", "}")
		}
		return m
	}
	for n, m := range d.Modules {
		s := svc{Image: m.Image, Command: m.Command, Environment: plain(m.Env), DependsOn: m.Uses}
		if m.Build != nil {
			b := map[string]any{"context": m.Build.Context}
			if m.Build.Dockerfile != "" {
				b["dockerfile"] = m.Build.Dockerfile
			}
			if m.Build.Target != "" {
				b["target"] = m.Build.Target
			}
			if len(m.Build.Args) > 0 {
				b["args"] = m.Build.Args
			}
			s.Build = b
			_ = os.MkdirAll(filepath.Join(root, m.Build.Context), 0o755)
		}
		for _, v := range m.Volumes {
			s.Volumes = append(s.Volumes, v)
			out.Volumes[strings.SplitN(v, ":", 2)[0]] = struct{}{}
		}
		out.Services[n] = s
	}
	for n, sv := range d.Services {
		s := svc{Image: sv.Image, Command: sv.Command, Environment: plain(sv.Env), DependsOn: sv.Uses}
		if sv.Volume != "" {
			s.Volumes = []string{n + "-data:" + sv.Volume}
			out.Volumes[n+"-data"] = struct{}{}
		}
		out.Services[n] = s
	}
	data, _ := yaml.Marshal(out)
	file := filepath.Join(root, "devopus-export.yml")
	_ = os.WriteFile(file, data, 0o644)
	cmd := exec.Command("docker", "compose", "-f", file, "config", "-q")
	cmd.Dir = root
	if o, err := cmd.CombinedOutput(); err != nil {
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(o)), "\n") {
			if !strings.Contains(l, "level=warning") {
				lines = append(lines, l)
			}
		}
		return "ошибка: " + firstLine(strings.Join(lines, " "))
	}
	return "ok"
}

func render(rs []*Report) string {
	var b strings.Builder
	levels := map[string]string{"A": "A — без правок", "B": "B — валиден, нужен обзор", "C": "C — нужны правки",
		"N": "N — не целевой: нет кода для разработки"}
	for _, g := range []struct{ key, title string }{{"product", "Реальные продукты"}, {"sample", "Типовые стеки (Docker awesome-compose)"}} {
		fmt.Fprintf(&b, "\n### %s\n\n| Проект | Уровень | Модули / сервисы | Маршруты (догадка) | Секреты → `secrets` | Код → worktree | Что смотреть руками | Экспорт обратно |\n|---|---|---|---|---|---|---|---|\n", g.title)
		count := map[string]int{}
		for _, r := range rs {
			if groupOf(r.Name) != g.key {
				continue
			}
			lvl := r.Level()
			count[lvl]++
			if r.LoadError != "" {
				fmt.Fprintf(&b, "| %s | %s | — | — | — | — | загрузка: %s | — |\n", r.Name, levels[lvl], esc(r.LoadError))
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %d / %d | %s | %d | %d | %s | %s |\n", r.Name, levels[lvl], r.Modules, r.Services,
				esc(strings.Join(r.Routes, "; ")), len(r.Secrets), r.CodeMounts, esc(attention(r)), esc(r.RoundTrip))
		}
		fmt.Fprintf(&b, "\nИтого: A — %d, B — %d, C — %d, не целевых — %d.\n", count["A"], count["B"], count["C"], count["N"])
	}
	fmt.Fprintf(&b, "\n### Подробности\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "\n**%s** (%s)\n", r.Name, levels[r.Level()])
		item := func(title string, list []string) {
			if len(list) > 0 {
				fmt.Fprintf(&b, "- %s: %s\n", title, esc(strings.Join(list, "; ")))
			}
		}
		if r.LoadError != "" {
			item("ошибка загрузки", []string{r.LoadError})
		}
		item("блокирует", r.Errors)
		item("маршруты", r.Routes)
		item("закреплено по digest", r.Pinned)
		item("закрепить позже (реестр ответил 429)", r.PinLater)
		item("в профилях", r.InProfiles)
		item("секреты", r.Secrets)
		item("переменные хоста", r.HostVars)
		item("bind файлов репозитория", r.RepoMounts)
		item("bind вне репозитория", r.HostMounts)
		item("переименовано", r.Renamed)
		var keys []string
		for k, v := range r.Unsupported {
			keys = append(keys, k+": "+strings.Join(v, ", "))
		}
		sort.Strings(keys)
		item("нет в формате v1", keys)
		if r.RoundTrip != "" {
			item("экспорт обратно", []string{r.RoundTrip})
		}
	}
	return b.String()
}

func attention(r *Report) string {
	var parts []string
	if len(r.Errors) > 0 {
		parts = append(parts, "**блокирует:** "+strings.Join(r.Errors, "; "))
	}
	if len(r.InProfiles) > 0 {
		parts = append(parts, fmt.Sprintf("в профилях: %d", len(r.InProfiles)))
	}
	if len(r.HostVars) > 0 {
		parts = append(parts, "переменные хоста: "+strings.Join(r.HostVars, ", "))
	}
	if len(r.HostMounts) > 0 {
		parts = append(parts, fmt.Sprintf("bind вне репозитория: %d", len(r.HostMounts)))
	}
	if len(r.RepoMounts) > 0 {
		parts = append(parts, fmt.Sprintf("bind файлов репозитория: %d", len(r.RepoMounts)))
	}
	if len(r.Pinned) > 0 {
		parts = append(parts, fmt.Sprintf("закреплено по digest: %d", len(r.Pinned)))
	}
	if len(r.PinLater) > 0 {
		parts = append(parts, fmt.Sprintf("закрепить позже (429 реестра): %d", len(r.PinLater)))
	}
	if len(r.Renamed) > 0 {
		parts = append(parts, fmt.Sprintf("переименовано: %d", len(r.Renamed)))
	}
	var keys []string
	for k, v := range r.Unsupported {
		keys = append(keys, fmt.Sprintf("%s (%d)", k, len(v)))
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		parts = append(parts, "нет в формате v1: "+strings.Join(keys, ", "))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

func groupOf(name string) string {
	for _, s := range corpus {
		if s.Name == name {
			return s.Group
		}
	}
	return ""
}

func esc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

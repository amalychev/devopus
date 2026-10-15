package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The prototype drives Docker through the CLI and generated compose files; the real agent
// will use the Docker Engine API and compose-go (docs/architecture/tech-stack.md).

const (
	labelProject = "com.devopus.project"
	labelEnv     = "com.devopus.env"
	labelModule  = "com.devopus.module"
	envoyName    = "devopus-envoy"
)

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func moduleImage(ws *Workspace, name string) string {
	m := ws.Modules[name]
	if m.Image != "" {
		return m.Image
	}
	return fmt.Sprintf("devopus-proto/%s-%s:dev", ws.Project, name)
}

// BuildImages builds module images once; features reuse them and mount their own worktree.
func BuildImages(st *State, ws *Workspace) error {
	for _, name := range ws.ModuleNames() {
		m := ws.Modules[name]
		if m.Build == "" {
			continue
		}
		if _, err := docker("build", "-q", "-t", moduleImage(ws, name), filepath.Join(st.Root, m.Build)); err != nil {
			return err
		}
	}
	return nil
}

func ensureNetwork(st *State, env *Env) error {
	name := st.NetworkName(env)
	if _, err := docker("network", "inspect", name); err == nil {
		return nil
	}
	_, err := docker("network", "create",
		"--subnet", env.Subnet,
		"--label", labelProject+"="+st.Project,
		"--label", labelEnv+"="+env.ID,
		name)
	return err
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
	Volumes  map[string]composeVolume  `yaml:"volumes,omitempty"`
}

type composeService struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	Command       []string          `yaml:"command,omitempty"`
	WorkingDir    string            `yaml:"working_dir,omitempty"`
	Environment   map[string]string `yaml:"environment,omitempty"`
	Volumes       []string          `yaml:"volumes,omitempty"`
	Labels        map[string]string `yaml:"labels"`
	Networks      []string          `yaml:"networks"`
	Restart       string            `yaml:"restart"`
}

type composeNetwork struct {
	External bool   `yaml:"external"`
	Name     string `yaml:"name"`
}

type composeVolume struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}

// Compose renders one environment. Logical names (payments, db) are compose service keys,
// so Docker DNS resolves them inside the env network. Shared base modules/services are not
// part of this file: their names are network aliases of Envoy (see envoy.go).
// No module publishes ports to the host; only Envoy does.
func Compose(st *State, ws *Workspace, env *Env) composeFile {
	labels := func(extra string) map[string]string {
		l := map[string]string{labelProject: st.Project, labelEnv: env.ID}
		if extra != "" {
			l[labelModule] = extra
		}
		return l
	}
	cf := composeFile{
		Name:     st.ComposeProject(env),
		Services: map[string]composeService{},
		Networks: map[string]composeNetwork{"env": {External: true, Name: st.NetworkName(env)}},
		Volumes:  map[string]composeVolume{},
	}

	for _, name := range env.Services {
		s := ws.Services[name]
		svc := composeService{
			Image:         s.Image,
			ContainerName: st.Container(env, name),
			Environment:   s.Env,
			Labels:        labels(name),
			Networks:      []string{"env"},
			Restart:       "unless-stopped",
		}
		if s.Volume != "" {
			vol := name + "-data"
			cf.Volumes[vol] = composeVolume{
				Name:   fmt.Sprintf("%s-%s-%s", st.Project, env.ID, name),
				Labels: labels(name),
			}
			svc.Volumes = []string{vol + ":" + s.Volume}
		}
		cf.Services[name] = svc
	}

	for _, name := range env.Modules {
		m := ws.Modules[name]
		vars := map[string]string{}
		for k, v := range m.Env {
			vars[k] = v
		}
		vars["DEVOPUS_ENV_ID"] = env.ID
		vars["DEVOPUS_ENV_KIND"] = env.Kind
		vars["DEVOPUS_ENV_SLUG"] = env.Slug
		vars["DEVOPUS_ENV_URL"] = st.URL(env, ws)
		vars["PORT"] = fmt.Sprint(m.Port)
		cf.Services[name] = composeService{
			Image:         moduleImage(ws, name),
			ContainerName: st.Container(env, name),
			Command:       []string{"sh", "-c", m.Command},
			WorkingDir:    "/app",
			Environment:   vars,
			Volumes:       []string{filepath.Join(env.Code, m.Path) + ":/app"},
			Labels:        labels(name),
			Networks:      []string{"env"},
			Restart:       "unless-stopped",
		}
	}
	return cf
}

func composeUp(st *State, ws *Workspace, env *Env) error {
	if err := ensureNetwork(st, env); err != nil {
		return err
	}
	cf := Compose(st, ws, env)
	data, err := yaml.Marshal(cf)
	if err != nil {
		return err
	}
	dir := filepath.Join(homeDir(), "envs", env.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	_, err = docker("compose", "-p", cf.Name, "-f", file, "up", "-d", "--remove-orphans")
	return err
}

func composeDown(st *State, env *Env, volumes bool) error {
	file := filepath.Join(homeDir(), "envs", env.ID, "compose.yaml")
	args := []string{"compose", "-p", st.ComposeProject(env), "-f", file, "down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	if _, err := os.Stat(file); err == nil {
		if _, err := docker(args...); err != nil {
			return err
		}
	}
	_, _ = docker("network", "disconnect", "-f", st.NetworkName(env), envoyName)
	_, _ = docker("network", "rm", st.NetworkName(env))
	return os.RemoveAll(filepath.Join(homeDir(), "envs", env.ID))
}

// runMigrations runs hooks.migrate inside each module of the env that owns its data.
// Migrations of a feature only ever touch the feature's own services (docs/architecture/data.md).
func runMigrations(st *State, ws *Workspace, env *Env, log func(string, ...any)) error {
	for _, name := range env.Modules {
		m := ws.Modules[name]
		if m.Hooks.Migrate == "" {
			continue
		}
		if env.Kind == KindFeature && usesShared(ws, env, name) {
			log("  skip migrate for %s: it would run on a database shared with main", name)
			continue
		}
		var lastErr error
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if _, lastErr = docker("exec", st.Container(env, name), "sh", "-c", m.Hooks.Migrate); lastErr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if lastErr != nil {
			return fmt.Errorf("migrate %s: %w", name, lastErr)
		}
		log("  migrated %s", name)
	}
	return nil
}

func usesShared(ws *Workspace, env *Env, module string) bool {
	for _, u := range ws.Modules[module].Uses {
		if _, isSvc := ws.Services[u]; isSvc && !contains(env.Services, u) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

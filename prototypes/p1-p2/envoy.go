package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// One Envoy per device (adr/0004). The prototype renders a static config and restarts
// Envoy; the agent will push the same routes over xDS/SDS without restarts.
//
//   external listeners  :8080 / :8443 (TLS) — Host picks the environment, the longest
//                       route prefix picks the module: the env's own copy if it runs one,
//                       otherwise the base copy.
//   internal listeners  <envoy-ip-in-feature-net>:80 and :<service port> — Envoy holds the
//                       logical names of shared modules/services as network aliases in each
//                       feature network and forwards them to the base environment.

const envoyImage = "envoyproxy/envoy:v1.34.1"

type m = map[string]any

func cluster(name, host string, port int) m {
	return m{
		"name":              name,
		"type":              "STRICT_DNS",
		"connect_timeout":   "2s",
		"dns_lookup_family": "V4_ONLY",
		"dns_refresh_rate":  "2s",
		"load_assignment": m{
			"cluster_name": name,
			"endpoints": []m{{"lb_endpoints": []m{{"endpoint": m{"address": m{
				"socket_address": m{"address": host, "port_value": port},
			}}}}}},
		},
	}
}

func hcm(statPrefix string, routeConfig m) m {
	return m{
		"name": "envoy.filters.network.http_connection_manager",
		"typed_config": m{
			"@type":        "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
			"stat_prefix":  statPrefix,
			"route_config": routeConfig,
			"http_filters": []m{{
				"name":         "envoy.filters.http.router",
				"typed_config": m{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"},
			}},
		},
	}
}

func route(prefix, clusterName string) m {
	return m{"match": m{"prefix": prefix}, "route": m{"cluster": clusterName, "timeout": "60s"}}
}

func envCluster(env *Env, kind, name string) string { return env.ID + "-" + kind + "-" + name }

// ownerOf returns the env that serves `name` for `env`: its own copy or the base one.
func ownerOf(base, env *Env, name string, isService bool) *Env {
	list := env.Modules
	if isService {
		list = env.Services
	}
	if contains(list, name) {
		return env
	}
	return base
}

func EnvoyConfig(st *State, ws *Workspace) (m, error) {
	base := st.Base()
	if base == nil {
		return nil, fmt.Errorf("base environment is missing")
	}
	var clusters []m
	for _, env := range st.Envs {
		for _, name := range env.Modules {
			clusters = append(clusters, cluster(envCluster(env, "mod", name), st.Container(env, name), ws.Modules[name].Port))
		}
		for _, name := range env.Services {
			s := ws.Services[name]
			clusters = append(clusters, cluster(envCluster(env, "svc", name), st.Container(env, name), s.Port))
			if s.UIPort != 0 {
				clusters = append(clusters, cluster(envCluster(env, "ui", name), st.Container(env, name), s.UIPort))
			}
		}
	}

	// Longest prefix first so /api wins over /.
	type rt struct{ prefix, module string }
	var routes []rt
	for _, name := range ws.ModuleNames() {
		for _, p := range ws.Modules[name].Routes {
			routes = append(routes, rt{p, name})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].prefix) > len(routes[j].prefix) })

	var vhosts []m
	for _, env := range st.Envs {
		host := st.Host(env, ws)
		var rs []m
		for _, r := range routes {
			owner := ownerOf(base, env, r.module, false)
			rs = append(rs, route(r.prefix, envCluster(owner, "mod", r.module)))
		}
		vhosts = append(vhosts, m{"name": env.ID, "domains": []string{host, host + ":*"}, "routes": rs})
		for _, name := range ws.ServiceNames() {
			s := ws.Services[name]
			if s.Expose == "" || s.UIPort == 0 {
				continue
			}
			owner := ownerOf(base, env, name, true)
			h := s.Expose + "." + host
			vhosts = append(vhosts, m{
				"name": env.ID + "-" + name, "domains": []string{h, h + ":*"},
				"routes": []m{route("/", envCluster(owner, "ui", name))},
			})
		}
	}
	external := m{
		"name":          "external",
		"virtual_hosts": vhosts,
		// Only the proxy may set feature context (docs/architecture/routing.md).
		"request_headers_to_remove": []string{"baggage"},
	}

	listeners := []m{
		{
			"name":          "external_http",
			"address":       m{"socket_address": m{"address": "0.0.0.0", "port_value": 8080}},
			"filter_chains": []m{{"filters": []m{hcm("external_http", external)}}},
		},
		{
			"name":    "external_https",
			"address": m{"socket_address": m{"address": "0.0.0.0", "port_value": 8443}},
			"filter_chains": []m{{
				"filters": []m{hcm("external_https", external)},
				"transport_socket": m{
					"name": "envoy.transport_sockets.tls",
					"typed_config": m{
						"@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
						"common_tls_context": m{"tls_certificates": []m{{
							"certificate_chain": m{"filename": "/etc/docklet/tls/cert.pem"},
							"private_key":       m{"filename": "/etc/docklet/tls/key.pem"},
						}}},
					},
				},
			}},
		},
	}

	for _, env := range st.Envs {
		if env.Kind != KindFeature {
			continue
		}
		sharedMods, sharedSvcs := SharedFrom(ws, env)
		var vh []m
		for _, name := range sharedMods {
			vh = append(vh, m{
				"name": name, "domains": []string{name, name + ":*"},
				"routes": []m{route("/", envCluster(base, "mod", name))},
			})
		}
		if len(vh) > 0 {
			listeners = append(listeners, m{
				"name":          "internal_http_" + env.ID,
				"address":       m{"socket_address": m{"address": env.EnvoyIP, "port_value": 80}},
				"filter_chains": []m{{"filters": []m{hcm("internal_"+env.ID, m{"name": "internal_" + env.ID, "virtual_hosts": vh})}}},
			})
		}
		usedPorts := map[int]string{80: "http"}
		for _, name := range sharedSvcs {
			p := ws.Services[name].Port
			if other, taken := usedPorts[p]; taken {
				return nil, fmt.Errorf("feature %s: shared service %s and %s both need port %d on the Envoy alias", env.Slug, name, other, p)
			}
			usedPorts[p] = name
			listeners = append(listeners, m{
				"name":    fmt.Sprintf("internal_tcp_%s_%s", env.ID, name),
				"address": m{"socket_address": m{"address": env.EnvoyIP, "port_value": p}},
				"filter_chains": []m{{"filters": []m{{
					"name": "envoy.filters.network.tcp_proxy",
					"typed_config": m{
						"@type":       "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
						"stat_prefix": "tcp_" + env.ID + "_" + name,
						"cluster":     envCluster(base, "svc", name),
					},
				}}}},
			})
		}
	}

	return m{
		"static_resources": m{"listeners": listeners, "clusters": clusters},
		"admin":            m{"address": m{"socket_address": m{"address": "127.0.0.1", "port_value": 9901}}},
	}, nil
}

// ApplyEnvoy writes config + certs, wires Envoy into every env network with the right
// aliases and static IP, and (re)starts it.
func ApplyEnvoy(st *State, ws *Workspace) error {
	dir := filepath.Join(homeDir(), "envoy")
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		return err
	}
	var hosts []string
	for _, env := range st.Envs {
		h := st.Host(env, ws)
		hosts = append(hosts, h)
		for _, name := range ws.ServiceNames() {
			if e := ws.Services[name].Expose; e != "" {
				hosts = append(hosts, e+"."+h)
			}
		}
	}
	if err := IssueCert(filepath.Join(dir, "tls"), hosts); err != nil {
		return err
	}
	cfg, err := EnvoyConfig(st, ws)
	if err != nil {
		return err
	}
	data, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "envoy.yaml"), data, 0o644); err != nil {
		return err
	}

	running := false
	if out, err := docker("inspect", "-f", "{{.State.Running}}", envoyName); err == nil {
		running = out == "true"
	} else {
		_, err := docker("create", "--name", envoyName,
			"--label", labelProject+"="+st.Project,
			"-e", "ENVOY_UID=0",
			"-p", fmt.Sprintf("127.0.0.1:%d:8080", st.HTTPPort),
			"-p", fmt.Sprintf("127.0.0.1:%d:8443", st.HTTPSPort),
			"-v", dir+":/etc/docklet:ro",
			"--restart", "unless-stopped",
			envoyImage, "envoy", "-c", "/etc/docklet/envoy.yaml", "--log-level", "warn")
		if err != nil {
			return err
		}
	}

	// Touch only networks that are new or whose aliases changed. Re-attaching Envoy to a
	// network gives it a new MAC under the same IP, and on OrbStack containers kept sending
	// replies to the old MAC (stale ARP) until their cache expired.
	attached := envoyAliases()
	for _, env := range st.Envs {
		net := st.NetworkName(env)
		var aliases []string
		if env.Kind == KindFeature {
			sharedMods, sharedSvcs := SharedFrom(ws, env)
			aliases = append(sharedMods, sharedSvcs...)
		}
		if have, ok := attached[net]; ok && sameSet(have, aliases) {
			continue
		}
		_, _ = docker("network", "disconnect", "-f", net, envoyName)
		args := []string{"network", "connect", "--ip", env.EnvoyIP}
		for _, a := range aliases {
			args = append(args, "--alias", a)
		}
		args = append(args, net, envoyName)
		if _, err := docker(args...); err != nil {
			return err
		}
	}

	// A restart drops in-flight requests of every environment, not only the changed one;
	// the agent avoids this with xDS. Here we at least block until Envoy serves again.
	restartedAt := time.Now()
	if running {
		_, err = docker("restart", "-t", "1", envoyName)
	} else {
		_, err = docker("start", envoyName)
	}
	if err != nil {
		return err
	}
	if err := WaitReady(st, ws, st.Base(), 30*time.Second); err != nil {
		return err
	}
	LastEnvoyGap = time.Since(restartedAt)
	return nil
}

// LastEnvoyGap is how long the last config change kept Envoy from serving.
var LastEnvoyGap time.Duration

func RemoveEnvoy() {
	_, _ = docker("rm", "-f", envoyName)
}

func envoyLogsTail() string {
	out, _ := exec.Command("docker", "logs", "--tail", "20", envoyName).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// envoyAliases returns the networks Envoy is attached to and the aliases it has there
// (without Docker's automatic container-name and ID aliases).
func envoyAliases() map[string][]string {
	out, err := docker("inspect", "-f", `{{range $n, $e := .NetworkSettings.Networks}}{{$n}}={{join $e.Aliases ","}};{{end}}`, envoyName)
	res := map[string][]string{}
	if err != nil {
		return res
	}
	for _, part := range strings.Split(out, ";") {
		name, list, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			continue
		}
		var aliases []string
		for _, a := range strings.Split(list, ",") {
			if a != "" && a != envoyName && !strings.HasPrefix(envoyName, a) {
				aliases = append(aliases, a)
			}
		}
		res[name] = aliases
	}
	return res
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

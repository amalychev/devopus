package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	netName  = "p6net"
	subnet   = "172.31.6.0/24"
	envoyIP  = "172.31.6.2"
	dnsIP    = "172.31.6.3"
	envoyImg = "envoyproxy/envoy:v1.34.1"
)

// containerTests: inside a container *.localhost is the container's own loopback. CoreDNS
// in the env network answers *.localhost with Envoy's address and forwards everything
// else to Docker's DNS; the agent hands every container the CA without changing images.
func containerTests(p *pki) {
	section("containers", "Внутри контейнеров: CoreDNS + доверие к CA без пересборки образов")
	defer containerCleanup()
	containerCleanup()

	if err := writeContainerConfigs(p); err != nil {
		record("containers", "подготовка", false, err.Error())
		return
	}
	steps := [][]string{
		{"network", "create", "--subnet", subnet, netName},
		{"run", "-d", "--name", "p6-envoy", "--network", netName, "--ip", envoyIP, "-e", "ENVOY_UID=0",
			"-v", p.Dir + ":/etc/docklet:ro", envoyImg, "envoy", "-c", "/etc/docklet/envoy.yaml", "--log-level", "warn"},
		{"run", "-d", "--name", "p6-coredns", "--network", netName, "--ip", dnsIP,
			"-v", filepath.Join(p.Dir, "Corefile") + ":/Corefile:ro", "coredns/coredns:1.11.3", "-conf", "/Corefile"},
	}
	for _, s := range steps {
		if _, err := run("docker", s...); err != nil {
			record("containers", "подготовка", false, err.Error())
			return
		}
	}
	time.Sleep(2 * time.Second)

	// Resolution, checked with getaddrinfo (Python): curl does not ask DNS for *.localhost.
	resolve := func(dns bool, name string) (string, error) {
		a := []string{"run", "--rm", "--network", netName}
		if dns {
			a = append(a, "--dns", dnsIP)
		}
		a = append(a, "python:3.12-slim", "python", "-c", "import socket;print(socket.gethostbyname('"+name+"'))")
		return run("docker", a...)
	}
	out, err := resolve(false, "app.shop.localhost")
	record("containers", "без CoreDNS: app.shop.localhost (getaddrinfo)", err != nil, "не резолвится: "+oneLine(lastLine(out), err))
	out, err = resolve(true, "app.shop.localhost")
	record("containers", "с CoreDNS (--dns): app.shop.localhost", out == envoyIP, "→ "+oneLine(out, err))
	out, err = resolve(true, "pay-123.app.shop.localhost")
	record("containers", "с CoreDNS: любой уровень вложенности (pay-123.app.shop.localhost)", out == envoyIP, "→ "+oneLine(out, err))
	out, err = resolve(true, "p6-envoy")
	record("containers", "с CoreDNS: имена контейнеров по-прежнему резолвятся", out == envoyIP, "p6-envoy → "+oneLine(out, err))
	out, _ = run("docker", "run", "--rm", "--network", netName, "--dns", dnsIP, "curlimages/curl:8.11.0", "-sv", "--max-time", "2", "http://app.shop.localhost/")
	tried := "?"
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Trying") {
			tried = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "*"))
			break
		}
	}
	record("containers", "curl с CoreDNS: app.shop.localhost", false, "DNS не спрашивает, сразу "+tried+" (RFC 6761, так же Chromium)")

	// Trust.
	bundle := filepath.Join(p.Dir, "bundle.pem")
	public, _ := os.ReadFile("/etc/ssl/cert.pem") // the host's public roots
	own, _ := os.ReadFile(p.CAPath)
	_ = os.WriteFile(bundle, append(append(public, '\n'), own...), 0o644)

	clients := []struct {
		who, image string
		cmd        func(url string) []string
	}{
		{"curl", "curlimages/curl:8.11.0", func(u string) []string { return []string{"curl", "-sS", "--max-time", "5", u} }},
		{"Python urllib", "python:3.12-slim", func(u string) []string {
			return []string{"python", "-c", "import urllib.request;print(urllib.request.urlopen('" + u + "',timeout=5).read()[:20])"}
		}},
		{"Node fetch", "node:22-alpine", func(u string) []string {
			return []string{"node", "-e", "fetch('" + u + "').then(r=>r.text()).then(t=>console.log(t.slice(0,20))).catch(e=>{console.log(e.cause?.code||e.message);process.exit(1)})"}
		}},
		{"PHP file_get_contents", "php:8.3-cli", func(u string) []string {
			return []string{"php", "-r", `$r=@file_get_contents("` + u + `");if($r===false){echo error_get_last()["message"];exit(1);}echo substr($r,0,20);`}
		}},
	}
	modes := []struct {
		name string
		args []string
	}{
		{"без настройки", nil},
		{"агент: CA + env", []string{"-v", p.Dir + ":/docklet:ro",
			"-e", "SSL_CERT_FILE=/docklet/bundle.pem", // OpenSSL: curl, Python, PHP
			"-e", "NODE_EXTRA_CA_CERTS=/docklet/ca.pem", // Node keeps its own roots and adds ours
			"-e", "REQUESTS_CA_BUNDLE=/docklet/bundle.pem", // Python requests (certifi)
			"-e", "CURL_CA_BUNDLE=/docklet/bundle.pem"}},
	}
	for _, m := range modes {
		for _, c := range clients {
			try := func(url string) (string, error) {
				a := []string{"run", "--rm", "--network", netName, "--dns", dnsIP}
				a = append(a, m.args...)
				a = append(a, c.image)
				return run("docker", append(a, c.cmd(url)...)...)
			}
			out, err := try("https://app.shop.localhost/")
			wantOK := m.args != nil
			got := err == nil && strings.Contains(out, "ok")
			record("containers", fmt.Sprintf("%s, %s: https://app.shop.localhost", c.who, m.name), got == wantOK, oneLine(out, err))
			out, err = try("https://evil.example.com/")
			record("containers", fmt.Sprintf("%s, %s: https://evil.example.com (Name Constraints)", c.who, m.name), !(err == nil && strings.Contains(out, "ok")), "отклонено: "+oneLine(out, err))
			out, err = try("https://example.com/")
			record("containers", fmt.Sprintf("%s, %s: публичный https://example.com", c.who, m.name), err == nil, oneLine(out, err))
		}
	}
	// Workaround for clients that pin *.localhost to loopback (curl, headless Chromium):
	// a relay in the module's network namespace listens on 127.0.0.1:443 and forwards to Envoy.
	_, err = run("docker", "run", "-d", "--name", "p6-relay", "--network", netName, "alpine:3.20", "sh", "-c",
		"apk add -q socat >/dev/null && exec socat TCP-LISTEN:443,bind=127.0.0.1,fork,reuseaddr TCP:"+envoyIP+":443")
	if err == nil {
		time.Sleep(3 * time.Second)
		out, err = run("docker", "run", "--rm", "--network", "container:p6-relay", "-v", p.Dir+":/docklet:ro",
			"-e", "CURL_CA_BUNDLE=/docklet/bundle.pem", "curlimages/curl:8.11.0", "-sS", "--max-time", "5", "https://app.shop.localhost/")
	}
	record("containers", "curl + relay 127.0.0.1:443 → Envoy в сетевом пространстве модуля", err == nil && strings.Contains(out, "ok"), oneLine(out, err))
}

func writeContainerConfigs(p *pki) error {
	corefile := fmt.Sprintf(`.:53 {
    # *.localhost and the test name point at Envoy; AAAA for them is empty on purpose.
    template IN A localhost {
        answer "{{ .Name }} 60 IN A %[1]s"
    }
    template IN AAAA localhost {
        rcode NOERROR
    }
    template IN A evil.example.com {
        answer "{{ .Name }} 60 IN A %[1]s"
    }
    # Container names and the outside world: Docker's own DNS inside this container.
    forward . 127.0.0.11
}
`, envoyIP)
	if err := os.WriteFile(filepath.Join(p.Dir, "Corefile"), []byte(corefile), 0o644); err != nil {
		return err
	}
	chain := func(name, cert, key string) string {
		return fmt.Sprintf(`
    - filter_chain_match: { server_names: ["%s"] }
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          common_tls_context:
            tls_certificates: [{ certificate_chain: { filename: /etc/docklet/%s }, private_key: { filename: /etc/docklet/%s } }]
      filters: [*hcm]`, name, filepath.Base(cert), filepath.Base(key))
	}
	envoy := `static_resources:
  listeners:
  - name: http
    address: { socket_address: { address: 0.0.0.0, port_value: 80 } }
    filter_chains:
    - filters:
      - &hcm
        name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: p6
          http_filters: [{ name: envoy.filters.http.router, typed_config: { "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router } }]
          route_config:
            virtual_hosts: [{ name: all, domains: ["*"], routes: [{ match: { prefix: / }, direct_response: { status: 200, body: { inline_string: ok } } }] }]
  - name: https
    address: { socket_address: { address: 0.0.0.0, port_value: 443 } }
    listener_filters: [{ name: envoy.filters.listener.tls_inspector, typed_config: { "@type": type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector } }]
    filter_chains:` + chain("app.shop.localhost", p.GoodPEM[0], p.GoodPEM[1]) + chain("evil.example.com", p.EvilPEM[0], p.EvilPEM[1]) + "\n"
	return os.WriteFile(filepath.Join(p.Dir, "envoy.yaml"), []byte(envoy), 0o644)
}

func containerCleanup() {
	_, _ = run("docker", "rm", "-f", "p6-envoy", "p6-coredns", "p6-relay")
	_, _ = run("docker", "network", "rm", netName)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

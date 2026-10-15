package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/docklet/docklet/internal/agent/pki"
	"github.com/docklet/docklet/internal/agent/runtime"
)

func TestResourcesAreConsistentAndStable(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := ca.Issue([]string{"shop.localhost", "mail.shop.localhost"})
	site := Site{Name: "abc123", Project: "shop", Hosts: leaf.Hosts, CertPEM: leaf.CertPEM, KeyPEM: leaf.KeyPEM, Page: Page{Status: 200, Title: "shop", Text: "up"}}

	empty := Config{HTTPSPort: 443}
	res, v0 := consistent(t, empty)
	if len(res[resource.ListenerType]) != 1 {
		t.Errorf("without sites only the HTTP listener exists, got %d", len(res[resource.ListenerType]))
	}
	// Coexistence without sites: TLS passes through, no HTTPS routes.
	consistent(t, Config{HTTPSPort: 443, Coexist: &Coexist{HTTPPort: 8080, HTTPSPort: 8443}})

	cfg := Config{HTTPSPort: 18443, Sites: []Site{site}}
	_, v1 := consistent(t, cfg)
	if _, again, _ := cfg.Resources(); again != v1 {
		t.Error("the same configuration got another version")
	}
	if v0 == v1 {
		t.Error("adding a site did not change the version")
	}
	cfg.Coexist = &Coexist{HTTPPort: 8080, HTTPSPort: 8443}
	res, v2 := consistent(t, cfg)
	if v2 == v1 {
		t.Fatal("coexistence did not change the version")
	}
	if len(res[resource.ClusterType]) != 2 {
		t.Errorf("coexist clusters: %d", len(res[resource.ClusterType]))
	}
	if _, err := Bootstrap(12345, mustControl(t)); err != nil {
		t.Fatal(err)
	}
}

// TestEnvoyServesSites runs the pinned Envoy against a real xDS server: the site is
// served over TLS with the local CA, HTTP redirects to the HTTPS port, and replacing
// the configuration does not restart anything. Skipped without a container runtime.
func TestEnvoyServesSites(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	ctl := mustControl(t)
	x, err := ListenXDS("127.0.0.1:0", ctl)
	if err != nil {
		t.Fatal(err)
	}
	go x.Serve()
	defer x.Close()

	ports := Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	spec, err := ContainerSpec(fmt.Sprintf("docklet-proxy-test-%d", ports.HTTPS), map[string]string{runtime.LabelManaged: "true", runtime.LabelInstance: "test"}, ports, x.Port(), ctl)
	if err != nil {
		t.Fatal(err)
	}
	spec.Restart = ""
	if err := rt.EnsureImage(ctx, Image); err != nil {
		t.Fatal(err)
	}
	id, err := rt.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(rt.ContainerLogs(context.Background(), id, 40))
		}
		_ = rt.RemoveContainer(context.Background(), id)
	})
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := ca.Issue([]string{"shop.localhost"})
	cfg := Config{HTTPSPort: uint32(ports.HTTPS), Sites: []Site{{Name: "e1", Hosts: leaf.Hosts, CertPEM: leaf.CertPEM, KeyPEM: leaf.KeyPEM, Page: Page{Status: 200, Title: "shop", Text: "first"}}}}
	if _, err := x.Set(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Root)
	body := waitBody(t, ctx, ports.HTTPS, "shop.localhost", roots, "first")
	if !strings.Contains(body, "first") {
		t.Fatalf("body: %s", body)
	}
	if st := x.State(); !st.Connected || st.Rejected != "" {
		t.Errorf("xds state: %+v", st)
	}

	// HTTP redirects to the HTTPS port.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/a?b=1", ports.HTTP), nil)
	req.Host = "shop.localhost"
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if want := fmt.Sprintf("https://shop.localhost:%d/a?b=1", ports.HTTPS); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != want {
		t.Errorf("redirect: %d %q, want %q", res.StatusCode, res.Header.Get("Location"), want)
	}

	// A second site arrives without disturbing the first: same Envoy, new chain.
	leaf2, _ := ca.Issue([]string{"pay.shop.localhost"})
	cfg.Sites = append(cfg.Sites, Site{Name: "e2", Hosts: leaf2.Hosts, CertPEM: leaf2.CertPEM, KeyPEM: leaf2.KeyPEM, Page: Page{Status: 200, Title: "pay", Text: "second"}})
	if _, err := x.Set(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	waitBody(t, ctx, ports.HTTPS, "pay.shop.localhost", roots, "second")
	waitBody(t, ctx, ports.HTTPS, "shop.localhost", roots, "first")

	// Coexistence: a dev server on the host gets every host Docklet does not serve,
	// HTTP as is and TLS untouched (its own certificate reaches the client).
	devHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dev server: "+r.Host) }))
	defer devHTTP.Close()
	devTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dev tls: "+r.Host) }))
	defer devTLS.Close()
	cfg.Coexist = &Coexist{HTTPPort: portOf(devHTTP.URL), HTTPSPort: portOf(devTLS.URL)}
	if _, err := x.Set(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var plain string
	for i := 0; i < 50 && !strings.Contains(plain, "dev server"); i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", ports.HTTP), nil)
		req.Host = "herd.test"
		if res, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			plain = string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if plain != "dev server: herd.test" {
		t.Errorf("coexist http: %q", plain)
	}
	devRoots := x509.NewCertPool()
	devRoots.AddCert(devTLS.Certificate())
	// httptest's certificate is for example.com; the SNI decides where Envoy sends it.
	if body := get(ctx, fmt.Sprintf("127.0.0.1:%d", ports.HTTPS), "example.com", devRoots); !strings.Contains(body, "dev tls: example.com") {
		t.Errorf("coexist tls: %q", body)
	}
	waitBody(t, ctx, ports.HTTPS, "shop.localhost", roots, "first")

	// Over IPv6 loopback too; Colima needs the agent's relay for that.
	rel := &Relays{}
	defer rel.Close()
	if r := rel.Ensure(ports); len(r) > 0 {
		t.Logf("relays: %v", r)
	}
	if body := get(ctx, fmt.Sprintf("[::1]:%d", ports.HTTPS), "shop.localhost", roots); !strings.Contains(body, "first") {
		t.Errorf("::1: %q", body)
	}
}

func waitBody(t *testing.T, ctx context.Context, port uint16, host string, roots *x509.CertPool, want string) string {
	t.Helper()
	var last string
	for ctx.Err() == nil {
		last = get(ctx, fmt.Sprintf("127.0.0.1:%d", port), host, roots)
		if strings.Contains(last, want) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never served %q; last: %s", host, want, last)
	return ""
}

func get(ctx context.Context, addr, host string, roots *x509.CertPool) string {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	res, err := c.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return fmt.Sprintf("%d %s %s", res.StatusCode, res.Header.Get("server"), b)
}

// consistent builds the resources and checks them the way the snapshot cache does.
func consistent(t *testing.T, cfg Config) (map[resource.Type][]types.Resource, string) {
	t.Helper()
	res, v, err := cfg.Resources()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cache.NewSnapshot(v, res)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Consistent(); err != nil {
		t.Fatalf("%+v: %v", cfg, err)
	}
	return res, v
}

func portOf(rawURL string) uint32 {
	u, _ := url.Parse(rawURL)
	p, _ := strconv.Atoi(u.Port())
	return uint32(p)
}

func mustControl(t *testing.T) *pki.Control {
	t.Helper()
	c, err := pki.LoadOrCreateControl(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

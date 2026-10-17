// Package agent runs the local Devopus agent: the desired-state store, the
// reconcile loop and the local API on a unix socket (docs/architecture/agent.md).
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fallback "golang.org/x/crypto/x509roots/fallback/bundle"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/gen/devopus/agent/v2/agentv2connect"
	"github.com/amalychev/devopus/internal/agent/acme"
	"github.com/amalychev/devopus/internal/agent/api"
	"github.com/amalychev/devopus/internal/agent/lifecycle"
	"github.com/amalychev/devopus/internal/agent/link"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/pki"
	"github.com/amalychev/devopus/internal/agent/proxy"
	"github.com/amalychev/devopus/internal/agent/reconcile"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/daemon"
	"github.com/amalychev/devopus/internal/secrets"
	"github.com/amalychev/devopus/internal/spec"
)

// Intervals of the reconcile loop: a periodic pass catches changes made behind the
// agent's back (a network removed by hand); after a failure it retries sooner.
const (
	resyncInterval = 30 * time.Second
	retryInterval  = 5 * time.Second
	// waitInterval: applications are still starting; look again soon.
	waitInterval = 2 * time.Second
	// passTimeout bounds one pass: clones, builds and installs can take minutes.
	passTimeout = 30 * time.Minute
)

type Options struct {
	Version string
	Log     *slog.Logger
	// ProxyPorts pins the proxy's host ports instead of probing 80/443; for tests that
	// run next to a real agent.
	ProxyPorts *proxy.Ports
	// ACMEDirectory and ACMEClient reach another certificate authority than Let's
	// Encrypt (its staging, a test CA); $DEVOPUS_ACME_DIRECTORY sets the first too.
	ACMEDirectory string
	ACMEClient    *http.Client
}

type Agent struct {
	opt   Options
	log   *slog.Logger
	store *store.Store
	rt    *runtime.Docker
	rec   *reconcile.Reconciler
	hub   *api.Hub
	ca    *pki.CA

	life *lifecycle.Manager
	// creds keeps the access token of the user signed in on this device.
	creds *credentials.Keeper
	// link brings the projects of the current workspace from its server.
	link *link.Link
	// kick asks the loop for a pass now.
	kick chan struct{}

	mu      sync.Mutex
	rtInfo  *agentv2.RuntimeInfo
	lastErr error
}

// Sleep puts a feature to sleep in the background.
func (a *Agent) Sleep(envID string) error {
	if !a.life.Known(envID) {
		return errors.New("the feature is not running yet")
	}
	go func() {
		if err := a.life.Sleep(context.Background(), envID, lifecycle.ReasonManual); err != nil {
			a.log.Warn("sleep", "env", envID, "error", err)
		}
	}()
	return nil
}

// Wake wakes a feature in the background.
func (a *Agent) Wake(envID string) error {
	if !a.life.Known(envID) {
		return errors.New("the feature is not running yet")
	}
	a.life.Wake(envID)
	return nil
}

func (a *Agent) kickLoop() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *Agent) Containers() api.Containers             { return a.rt }
func (a *Agent) EnvsDir() string                        { return paths.Envs() }
func (a *Agent) Store() *store.Store                    { return a.store }
func (a *Agent) Statuses() []*agentv2.EnvironmentStatus { return a.rec.Statuses() }
func (a *Agent) Version() string                        { return a.opt.Version }
func (a *Agent) Force(envID string)                     { a.rec.Force(envID) }

func (a *Agent) Unsaved(ctx context.Context, env *agentv2.EnvironmentSpec, ws *spec.Project) (map[string]string, error) {
	return a.rec.Unsaved(ctx, env, ws)
}
func (a *Agent) Proxy() *agentv2.ProxyStatus { return a.rec.Proxy() }
func (a *Agent) Routes() []*agentv2.Route    { return a.rec.Routes() }

func (a *Agent) CA() *agentv2.CAInfo {
	return &agentv2.CAInfo{RootPath: a.ca.RootPath(), Fingerprint: a.ca.Fingerprint(), NotAfter: timestamppb.New(a.ca.Root.NotAfter)}
}

// watchOverrides starts a pass when a devopus.yaml (or the .gitignore next to it) of a
// workspace project changes: a personal override applies in seconds.
func (a *Agent) watchOverrides(ctx context.Context) {
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		h := sha256.New()
		for _, p := range a.store.Snapshot().Projects {
			if p.WorkspaceId == "" || p.Root == "" {
				continue
			}
			for _, f := range []string{store.OverrideFile(p), filepath.Join(p.Root, ".gitignore")} {
				b, _ := os.ReadFile(f)
				h.Write([]byte(f))
				h.Write(b)
			}
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if last != "" && sum != last {
			a.kickLoop()
		}
		last = sum
	}
}

// decryptSecrets opens a workspace project's secrets with the workspace's key, which
// reached this device wrapped for its own key (ADR-0009).
func decryptSecrets(p *agentv2.ProjectSpec) (map[string]string, error) {
	out := map[string]string{}
	if len(p.Secrets) == 0 {
		return out, nil
	}
	if len(p.KeyWrap) == 0 {
		names := make([]string, 0, len(p.Secrets))
		for n := range p.Secrets {
			names = append(names, n)
		}
		sort.Strings(names)
		who := "the workspace admin"
		if p.AdminEmail != "" {
			who += " (" + p.AdminEmail + ")"
		}
		return nil, fmt.Errorf("the workspace key has not reached this device yet, so %s cannot be read: the agent of %s or of a teamlead delivers it when online; the agent goes on as soon as it arrives",
			strings.Join(names, ", "), who)
	}
	dk, err := credentials.DeviceKey()
	if err != nil {
		return nil, err
	}
	key, err := secrets.Unwrap(p.KeyWrap, dk)
	if err != nil {
		return nil, err
	}
	for n, sealed := range p.Secrets {
		v, err := secrets.Open(sealed, key)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w (the key was rotated? the agent gets the new one from the server)", n, err)
		}
		out[n] = string(v)
	}
	return out, nil
}

// Account is the user signed in on this device, whose session the agent keeps fresh.
func (a *Agent) Account() *agentv2.AccountInfo {
	s := a.creds.Session()
	if s == nil {
		return nil
	}
	return &agentv2.AccountInfo{Email: s.Email, UserId: s.UserID, SessionId: s.SessionID, AuthUrl: s.AuthURL}
}

func (a *Agent) Link() *agentv2.LinkInfo {
	s := a.link.State()
	return &agentv2.LinkInfo{Workspace: s.Workspace, Server: s.Server, Connected: s.Connected, Error: s.Error}
}

func (a *Agent) Runtime() *agentv2.RuntimeInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rtInfo == nil {
		ep := a.rt.Endpoint()
		return &agentv2.RuntimeInfo{Name: ep.Name, Endpoint: ep.Host}
	}
	return a.rtInfo
}

// Run serves until ctx is done. Only one agent runs per data directory.
// ErrAlreadyRunning: another agent holds the lock of this data directory.
var ErrAlreadyRunning = daemon.ErrAlreadyRunning

func Run(ctx context.Context, opt Options) error {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	unlock, err := daemon.Lock(paths.Lock())
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(paths.State())
	if err != nil {
		return err
	}
	ep := runtime.Detect()
	rt, err := runtime.New(ep)
	if err != nil {
		return err
	}
	defer rt.Close()

	ca, err := pki.LoadOrCreateCA(paths.CA())
	if err != nil {
		return fmt.Errorf("local CA: %w", err)
	}
	ctl, err := pki.LoadOrCreateControl(paths.Proxy())
	if err != nil {
		return fmt.Errorf("proxy control certificates: %w", err)
	}
	// Containers reach the agent at host.docker.internal: the loopback on Docker
	// Desktop, OrbStack and Colima, the bridge's gateway on a Linux engine.
	host, err := rt.HostAddr(ctx)
	if err != nil {
		log.Warn("where containers reach the agent; using the loopback", "err", err)
		host = netip.MustParseAddr("127.0.0.1")
	}
	// An admin's server (ADR-0019): one workspace with its device token, signed
	// versions only, its environments public (R14).
	server, err := link.LoadServerConfig()
	if err != nil {
		return fmt.Errorf("%s: %w", link.ServerConfigFile(), err)
	}
	xds, err := listenXDS(ctl, host)
	if err != nil {
		return fmt.Errorf("xDS server: %w", err)
	}
	defer xds.Close()
	go func() {
		if err := xds.Serve(); err != nil {
			log.Error("xDS server", "error", err)
		}
	}()
	choose := proxy.ChoosePorts
	if server != nil && opt.ProxyPorts == nil {
		// A server serves the internet on 80 and 443, nothing else: no coexistence.
		opt.ProxyPorts = &proxy.Ports{HTTP: 80, HTTPS: 443, Public: true}
	}
	if opt.ProxyPorts != nil {
		fixed := *opt.ProxyPorts
		choose = func(context.Context) (proxy.Ports, []proxy.Conflict, error) { return fixed, nil, nil }
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Root)
	relays := &proxy.Relays{}
	defer relays.Close()

	a := &Agent{opt: opt, log: log, store: st, rt: rt, hub: api.NewHub(), ca: ca, kick: make(chan struct{}, 1), creds: &credentials.Keeper{Log: log}}
	go a.creds.Run(ctx)
	wakerLn, err := listenPinned(filepath.Join(paths.Proxy(), "waker-port"), host, lifecycle.ListenWaker)
	if err != nil {
		return fmt.Errorf("waker: %w", err)
	}
	direct := proxy.AnswersDirect(roots)
	proxyPorts := func() proxy.Ports {
		p := a.rec.Proxy()
		return proxy.Ports{HTTP: uint16(p.HttpPort), HTTPS: uint16(p.HttpsPort)}
	}
	a.life = lifecycle.New(rt, lifecycle.Options{
		Instance: paths.Instance(),
		File:     filepath.Join(paths.Home(), "lifecycle.json"),
		SetAwake: func(ctx context.Context, envID string, awake bool) error { return a.rec.SetAwake(ctx, envID, awake) },
		Answers: func(ctx context.Context, host, path string) error {
			return direct(ctx, host, path, proxyPorts())
		},
		Publish: func(envID string, fn func(*agentv2.EnvironmentStatus)) { a.rec.UpdateStatus(envID, fn) },
		Kick:    a.kickLoop,
		Log:     log,
	})
	xds.OnRequests(a.life.OnRequests)
	// A server's public environments get their certificates from Let's Encrypt; the
	// proxy sends the CA's challenges here.
	var certs *acme.Manager
	acmePort := 0
	if server != nil {
		dir := opt.ACMEDirectory
		if dir == "" {
			dir = os.Getenv("DEVOPUS_ACME_DIRECTORY")
		}
		certs = &acme.Manager{Dir: filepath.Join(paths.Home(), "acme"), Directory: dir, HTTPClient: opt.ACMEClient, Ready: a.kickLoop, Log: log}
		ln, err := listenPinned(filepath.Join(paths.Proxy(), "acme-port"), host, func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) })
		if err != nil {
			return fmt.Errorf("ACME answerer: %w", err)
		}
		acmePort = ln.Addr().(*net.TCPAddr).Port
		srv := &http.Server{Handler: certs, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
	}
	a.rec = reconcile.New(rt, reconcile.Options{
		Instance:    paths.Instance(),
		Decrypt:     decryptSecrets,
		ProxyName:   paths.ProxyName(),
		CA:          ca,
		Leaves:      pki.Leaves{Dir: paths.Certs()},
		Control:     ctl,
		XDS:         xds,
		ChoosePorts: choose,
		Probe:       proxy.Probe(roots),
		Relays:      relays,
		Answers:     proxy.Answers(roots),
		SourcesDir:  paths.Sources(),
		EnvsDir:     paths.Envs(),
		Bundle:      bundle(ca),
		Log:         log,
		Lifecycle:   a.life,
		WakerPort:   wakerLn.Addr().(*net.TCPAddr).Port,
		ACMEPort:    acmePort,
		PublicCert:  publicCert(certs),
	}, func(s *agentv2.EnvironmentStatus) {
		log.Info("environment", "id", s.Id, "project", s.Project, "phase", s.Phase.String(), "error", s.Error, "removed", s.Removed)
		a.hub.Publish(s)
	})

	changes, unsubscribe := a.hub.Subscribe()
	defer unsubscribe()
	if server != nil {
		st.Strict = true
		log.Info("the agent of a server", "server", server.Name, "workspace", server.WorkspaceName, "url", server.URL)
	}
	a.link = link.New(link.Options{Store: st, Token: a.creds.TokenFor, Statuses: a.rec.Statuses, Changed: changes, Version: opt.Version, Log: log, Server: server})
	go a.link.Run(ctx)
	go a.watchOverrides(ctx)

	waker := lifecycle.NewWaker(a.life, roots, proxyPorts)
	go func() {
		if err := waker.Serve(ctx, wakerLn); err != nil {
			log.Error("waker", "error", err)
		}
	}()
	go a.life.Run(ctx, 5*time.Second)

	ln, err := listen(paths.Socket())
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(agentv2connect.NewAgentServiceHandler(api.New(a, a.hub)))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	log.Info("agent started", "version", opt.Version, "socket", paths.Socket(), "runtime", ep.Name, "endpoint", ep.Host, "xds", xds.Port())

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		a.loop(ctx)
	}()

	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	a.hub.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-loopDone
	_ = os.Remove(paths.Socket())
	log.Info("agent stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// loop runs passes one at a time. A change of the desired state interrupts a running
// pass: a long install must not hold up 'devopus down' or another project's 'up'. Every
// step is idempotent, so the next pass picks up where the interrupted one stopped. A new
// revision with nothing else changed (a repeated 'devopus up') does not interrupt: it
// gets a pass of its own right after.
func (a *Agent) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.store.Changed():
		case <-a.kick:
		case <-timer.C:
		}
		desired := a.store.Snapshot()
		passCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- a.pass(passCtx, desired) }()
		var err error
		again := false
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case <-a.store.Changed():
				again = true
				if !sameState(desired, a.store.Snapshot()) {
					cancel()
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		next := resyncInterval
		switch {
		case again:
			next = 0
		case errors.Is(err, reconcile.ErrNotReady):
			next = waitInterval
		case err != nil:
			next = retryInterval
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// pass refreshes what the agent knows about the runtime and reconciles once.
func (a *Agent) pass(ctx context.Context, desired *agentv2.DesiredState) error {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	ep := a.rt.Endpoint()
	ri := &agentv2.RuntimeInfo{Name: ep.Name, Endpoint: ep.Host}
	info, err := a.rt.Info(ctx)
	if err == nil {
		ri.Name, ri.ServerVersion, ri.Os, ri.Reachable = info.Name, info.ServerVersion, info.OS, true
	} else {
		ri.Error = fmt.Sprintf("cannot reach %s at %s: %v", ep.Name, ep.Host, err)
	}
	a.mu.Lock()
	a.rtInfo = ri
	a.mu.Unlock()
	if err != nil {
		err = errors.New(ri.Error)
		a.rec.Fail(desired, err)
		a.report(err)
		return err
	}
	err = a.rec.Reconcile(ctx, desired)
	if errors.Is(err, reconcile.ErrNotReady) || (err != nil && ctx.Err() != nil) {
		a.report(nil)
		return err
	}
	a.report(err)
	return err
}

// sameState compares desired states without their revisions.
func sameState(a, b *agentv2.DesiredState) bool {
	a, b = proto.Clone(a).(*agentv2.DesiredState), proto.Clone(b).(*agentv2.DesiredState)
	a.Revision, b.Revision = 0, 0
	return proto.Equal(a, b)
}

// bundle is the public roots of the Mozilla store plus the local CA: applications point
// SSL_CERT_FILE and friends at it and trust both public sites and their environment.
func bundle(ca *pki.CA) []byte {
	var b []byte
	for root := range fallback.Roots() {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Certificate})...)
	}
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw})...)
}

// report logs an error once until it changes or clears.
func (a *Agent) report(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil && a.lastErr != nil:
		a.log.Info("reconcile recovered")
	case err != nil && (a.lastErr == nil || a.lastErr.Error() != err.Error()):
		a.log.Error("reconcile", "error", err)
	}
	a.lastErr = err
}

// listenPinned opens a loopback port and keeps it across agent restarts in file, so
// a restarted agent gives the proxy the same configuration.
func listenPinned(file string, host netip.Addr, listen func(addr string) (net.Listener, error)) (net.Listener, error) {
	if b, err := os.ReadFile(file); err == nil {
		if ln, err := listen(net.JoinHostPort(host.String(), strings.TrimSpace(string(b)))); err == nil {
			return ln, nil
		}
	}
	ln, err := listen(net.JoinHostPort(host.String(), "0"))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)+"\n"), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// listenXDS keeps the xDS port across agent restarts: it is baked into the proxy's
// bootstrap, and a new port would mean recreating the proxy.
func listenXDS(ctl *pki.Control, host netip.Addr) (*proxy.XDS, error) {
	file := filepath.Join(paths.Proxy(), "xds-port")
	if b, err := os.ReadFile(file); err == nil {
		if x, err := proxy.ListenXDS(net.JoinHostPort(host.String(), strings.TrimSpace(string(b))), ctl); err == nil {
			return x, nil
		}
	}
	x, err := proxy.ListenXDS(net.JoinHostPort(host.String(), "0"), ctl)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(strconv.Itoa(x.Port())+"\n"), 0o600); err != nil {
		x.Close()
		return nil, err
	}
	return x, nil
}

// listen opens the API socket, replacing a stale one left by a crashed agent. The lock
// already guarantees no other agent is serving it.
func listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// publicCert gives reconcile the certificates of public hosts, or nothing off a server.
func publicCert(m *acme.Manager) func(hosts []string) (*pki.Leaf, error) {
	if m == nil {
		return nil
	}
	return m.Cert
}

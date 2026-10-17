// Package link is the agent's connection to the workspace server (specs/agent-protocol.md):
// it watches the desired state of the device in the current workspace, puts it in the
// store, and reports what the device runs. The store keeps the last state it received:
// without the network the agent goes on with it, and environments keep running.
package link

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"filippo.io/age"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/secrets"
	"github.com/amalychev/devopus/internal/signing"
	"github.com/amalychev/devopus/internal/spec"
)

// Options of a link.
type Options struct {
	Store *store.Store
	// Token is the user's access token for a workspace server (credentials.Audience).
	Token func(ctx context.Context, audience string) (string, error)
	// Statuses are the device's environments now; Changed signals a change of them.
	Statuses func() []*agentv2.EnvironmentStatus
	Changed  <-chan *agentv2.EnvironmentStatus
	Version  string
	Log      *slog.Logger
	// Wait is how long the link waits for a workspace or a sign-in before looking
	// again; 10 s when zero.
	Wait time.Duration
	// Server makes this the agent of an admin's server: one workspace, its device token.
	Server *ServerConfig
}

// State is what the link is doing, for 'devopus status'.
type State struct {
	Workspace string
	Server    string
	Connected bool
	// Error is why it is not connected.
	Error string
	// Received is when the last desired state came.
	Received time.Time
}

// Link connects the agent to the workspace server.
type Link struct {
	opt Options
	mu  sync.Mutex
	st  State
}

func New(opt Options) *Link {
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	if opt.Wait == 0 {
		opt.Wait = 10 * time.Second
	}
	return &Link{opt: opt}
}

// State is a copy of the link's state.
func (l *Link) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *Link) set(fn func(*State)) {
	l.mu.Lock()
	fn(&l.st)
	l.mu.Unlock()
}

// ProjectsDir is where projects of the workspace server live on the device unless
// their folder is chosen: <data>/projects/<name>.
func ProjectsDir(name string) string { return filepath.Join(paths.Home(), "projects", name) }

// workspace is the one the link follows: the server's, or the device's current one.
func (l *Link) workspace() (*credentials.Workspace, error) {
	if c := l.opt.Server; c != nil {
		return &credentials.Workspace{ID: c.WorkspaceID, Name: c.WorkspaceName, ServerURL: c.URL}, nil
	}
	return credentials.CurrentWorkspace()
}

// token is the server's device token, or the signed-in person's access token.
func (l *Link) token(ctx context.Context, ws *credentials.Workspace) (string, error) {
	if c := l.opt.Server; c != nil {
		return c.Token, nil
	}
	return l.opt.Token(ctx, credentials.Audience(ws.ServerURL))
}

func root(name string) string {
	dir := ProjectsDir(name)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Run keeps the link up until ctx ends: it follows the device's current workspace and
// sign-in, and comes back after every break, waiting longer each time it fails.
func (l *Link) Run(ctx context.Context) {
	go l.report(ctx)
	failures := 0
	for ctx.Err() == nil {
		ws, err := l.workspace()
		if err != nil || ws == nil {
			l.set(func(s *State) { *s = State{Error: "no workspace chosen on this device: devopus workspace use <name>"} })
			l.sleep(ctx, l.opt.Wait)
			continue
		}
		tok, err := l.token(ctx, ws)
		if err != nil {
			l.set(func(s *State) {
				*s = State{Workspace: ws.Name, Server: ws.ServerURL, Error: "not signed in: " + err.Error()}
			})
			l.sleep(ctx, l.opt.Wait)
			continue
		}
		err = l.watch(ctx, ws, tok)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			failures = 0 // the token ran out or the workspace changed: again at once
			continue
		case connect.CodeOf(err) == connect.CodePermissionDenied:
			// Not a member any more: the workspace's projects stop here.
			if _, _, serr := l.opt.Store.SyncServer(ws.ID, "", store.ServerKey{}, nil, nil, root); serr != nil {
				l.opt.Log.Error("stop the workspace's projects", "err", serr)
			}
			l.opt.Log.Warn("the workspace server refused this device", "workspace", ws.Name, "err", err)
		default:
			l.opt.Log.Warn("workspace server link", "server", ws.ServerURL, "err", err)
		}
		l.set(func(s *State) { s.Connected, s.Error = false, err.Error() })
		failures++
		l.sleep(ctx, backoff(failures))
	}
}

// backoff is a full-jitter delay: 1 s doubling up to a minute, so agents that lost
// the server do not come back all at once.
func backoff(failures int) time.Duration {
	limit := time.Second << min(failures, 6)
	if limit > time.Minute {
		limit = time.Minute
	}
	return time.Duration(rand.Int64N(int64(limit))) + 500*time.Millisecond
}

func (l *Link) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func device(version string) *syncv1.Device {
	d := &syncv1.Device{Id: credentials.DeviceID(), Name: credentials.DeviceName(), AgentVersion: version}
	if k, err := credentials.DeviceKey(); err == nil {
		d.PublicKey = k.Recipient().String()
	}
	return d
}

// deliverKey wraps the workspace's key, which this device holds, for the devices
// waiting for it (ADR-0009, variant A): this is how a new member's agent gets the
// secrets with nobody doing anything.
func (l *Link) deliverKey(ctx context.Context, ws *credentials.Workspace, tok string, key store.ServerKey, pending []*syncv1.PendingDevice) {
	dk, err := credentials.DeviceKey()
	if err != nil {
		l.opt.Log.Warn("device key", "err", err)
		return
	}
	n, err := DeliverKey(ctx, ws.ServerURL, ws.ID, tok, key, dk, pending)
	if err != nil {
		l.opt.Log.Warn("deliver the workspace key", "err", err)
		return
	}
	l.opt.Log.Info("delivered the workspace key", "devices", n)
}

// DeliverKey opens the workspace's key wrapped for this device and wraps it for each
// pending device; it returns how many got it.
func DeliverKey(ctx context.Context, serverURL, workspaceID, tok string, key store.ServerKey, device *age.X25519Identity, pending []*syncv1.PendingDevice) (int, error) {
	wsKey, err := secrets.Unwrap(key.Wrapped, device)
	if err != nil {
		return 0, err
	}
	req := &workspacev1.ProvideKeyWrapsRequest{WorkspaceId: workspaceID, Generation: key.Generation}
	for _, d := range pending {
		wrapped, err := secrets.Wrap(wsKey, d.PublicKey)
		if err != nil {
			continue // a key that is not age's: the server checks keys, but trust no one
		}
		req.Wraps = append(req.Wraps, &workspacev1.KeyWrap{DeviceId: d.DeviceId, UserId: d.UserId, Wrapped: wrapped})
	}
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", "Bearer "+tok)
	res, err := workspacev1connect.NewWorkspaceServiceClient(http.DefaultClient, serverURL).ProvideKeyWraps(ctx, r)
	if err != nil {
		return 0, err
	}
	return int(res.Msg.Delivered), nil
}

func client(url string) syncv1connect.SyncServiceClient {
	// No timeout on the client: the watch stream lives as long as the token.
	return syncv1connect.NewSyncServiceClient(http.DefaultClient, url)
}

// watch follows the desired state until the stream ends, or the device switches to
// another workspace.
func (l *Link) watch(ctx context.Context, ws *credentials.Workspace, tok string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if l.opt.Server != nil {
			return // a server follows its one workspace
		}
		// Another workspace chosen on the device: end this stream.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if cur, err := credentials.CurrentWorkspace(); err == nil && (cur == nil || cur.ID != ws.ID || cur.ServerURL != ws.ServerURL) {
				cancel()
				return
			}
		}
	}()
	req := connect.NewRequest(&syncv1.WatchRequest{WorkspaceId: ws.ID, Device: device(l.opt.Version)})
	req.Header().Set("Authorization", "Bearer "+tok)
	stream, err := client(ws.ServerURL).Watch(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		m := stream.Msg()
		if m.Keepalive {
			continue
		}
		projects := make([]store.ServerProject, 0, len(m.Projects))
		for _, p := range m.Projects {
			projects = append(projects, store.ServerProject{Name: p.Name, Spec: []byte(p.ConfigYaml), Version: p.Version, Secrets: p.Secrets,
				OverridesAllowed: p.OverridesAllowed, OverridesPermitted: p.OverridesPermitted, Signature: p.Signature, SignerDevice: p.SignerDevice,
				Environment: l.environment(p.Environment)})
		}
		key := store.ServerKey{Generation: m.GetKey().GetGeneration(), Wrapped: m.GetKey().GetWrapped()}
		_, conflicts, err := l.opt.Store.SyncServer(m.WorkspaceId, m.AdminEmail, key, projects, TrustSet(m.WorkspaceId, m.Trust), root)
		if err != nil {
			return err
		}
		if w := l.opt.Store.Withheld(); len(w) > 0 {
			l.opt.Log.Warn("projects not taken: a server runs only versions signed by people the admin certified ('devopus trust approve')",
				"projects", strings.Join(w, "; "))
		}
		if len(conflicts) > 0 {
			l.opt.Log.Warn("projects of the workspace clash with projects of folders; 'devopus down --volumes --project <name>' frees the name",
				"projects", strings.Join(conflicts, ", "))
		}
		l.set(func(s *State) {
			*s = State{Workspace: m.WorkspaceName, Server: ws.ServerURL, Connected: true, Received: time.Now()}
		})
		l.opt.Log.Info("desired state from the workspace server", "workspace", m.WorkspaceName, "projects", len(m.Projects))
		if len(m.PendingDevices) > 0 && len(key.Wrapped) > 0 {
			l.deliverKey(ctx, ws, tok, key, m.PendingDevices)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return stream.Err()
}

// report tells the server what the device runs: after changes (gathered for a couple
// of seconds) and every minute.
func (l *Link) report(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var soon <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-l.opt.Changed:
			if !ok {
				return
			}
			if soon == nil {
				soon = time.After(2 * time.Second)
			}
			continue
		case <-soon:
			soon = nil
		case <-tick.C:
		}
		if err := l.Report(ctx); err != nil && !errors.Is(err, errNotLinked) {
			l.opt.Log.Debug("report to the workspace server", "err", err)
		}
	}
}

// environment is a server's environment of a project as the store checks it.
func (l *Link) environment(e *syncv1.ServerEnvironment) *store.ServerEnvironment {
	if e == nil || l.opt.Server == nil {
		return nil
	}
	return &store.ServerEnvironment{ServerID: l.opt.Server.ServerID, Kind: e.Kind, Domain: e.Domain, Signature: e.Signature, SignerDevice: e.SignerDevice}
}

// TrustSet is the server's word on signatures, as the store checks it: the root
// counts only until one is pinned, each certificate is checked against it.
func TrustSet(workspace string, t *syncv1.Trust) *signing.Set {
	if t == nil {
		return nil
	}
	set := &signing.Set{Workspace: workspace, Root: t.Root, RootDevice: t.RootDevice, RootEmail: t.RootEmail}
	for _, sg := range t.Signers {
		set.Signers = append(set.Signers, signing.Signer{DeviceID: sg.DeviceId, UserID: sg.UserId, Email: sg.Email, Public: sg.PublicKey, Certificate: sg.Certificate})
	}
	return set
}

var errNotLinked = errors.New("not linked to a workspace")

// Report sends the statuses of the current workspace's environments now.
func (l *Link) Report(ctx context.Context) error {
	ws, err := l.workspace()
	if err != nil || ws == nil || !l.State().Connected {
		return errNotLinked
	}
	tok, err := l.token(ctx, ws)
	if err != nil {
		return err
	}
	versions, pending := map[string]int32{}, map[string]int32{}
	overrides := map[string]spec.OverrideResult{}
	for _, p := range l.opt.Store.Snapshot().Projects {
		if p.WorkspaceId == ws.ID && !p.Orphaned {
			versions[p.Name] = p.ConfigVersion
			if p.Pending != nil {
				pending[p.Name] = p.Pending.Version
			}
			_, overrides[p.Name], _ = store.EffectiveProject(p)
		}
	}
	msg := &syncv1.ReportStatusRequest{WorkspaceId: ws.ID, Device: device(l.opt.Version)}
	for _, e := range l.opt.Statuses() {
		v, ok := versions[e.Project]
		if !ok || e.Removed {
			continue
		}
		kind := "base"
		if e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE {
			kind = "feature"
		}
		msg.Environments = append(msg.Environments, &syncv1.EnvironmentReport{
			Project: e.Project, Id: e.Id, Kind: kind, Name: e.Name, Branch: e.Branch,
			Phase: strings.ToLower(strings.TrimPrefix(e.Phase.String(), "PHASE_")), Url: e.Url, Error: e.Error, ConfigVersion: v,
			Overrides: overrides[e.Project].Applied, RejectedOverrides: overrides[e.Project].Rejected,
			PendingVersion: pending[e.Project],
		})
	}
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+tok)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = client(ws.ServerURL).ReportStatus(ctx, req)
	return err
}

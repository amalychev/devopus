package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
)

// The agents' link (specs/agent-protocol.md): an agent watches the desired state of its
// device in a workspace and reports what it runs. A change of a project's settings or
// of the members reaches the agents at once: every committed change wakes the watchers
// of its workspace. One server instance for now; several will pass the wake-ups over a
// bus (NATS) instead of memory.

// recheck is how often a watcher looks again without a wake-up: membership changes made
// elsewhere, and the token's expiry, are seen within it.
const recheck = 30 * time.Second

// onlineFor is how long a device counts as online after it was last seen.
const onlineFor = 2 * time.Minute

var _ syncv1connect.SyncServiceHandler = (*syncService)(nil)

// syncService is the agents' API; it shares the server's state and access check.
type syncService struct {
	syncv1connect.UnimplementedSyncServiceHandler
	s *Service
}

// hub wakes the watchers of a workspace.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool
}

func (h *hub) subscribe(ws string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan struct{}]bool{}
	}
	if h.subs[ws] == nil {
		h.subs[ws] = map[chan struct{}]bool{}
	}
	h.subs[ws][ch] = true
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[ws], ch)
		h.mu.Unlock()
	}
}

func (h *hub) wake(ws string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[ws] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// desired is what a member's device runs in a workspace: the projects they work in,
// with the secrets they may read, and the workspace's key wrapped for the device.
func (s *Service) desired(ctx context.Context, c *caller, device string) (*syncv1.WatchResponse, error) {
	w, err := s.q.Workspace(ctx, c.workspace)
	if err != nil {
		return nil, err
	}
	admin, err := s.q.WorkspaceAdmin(ctx, c.workspace)
	if err != nil {
		return nil, err
	}
	out := &syncv1.WatchResponse{WorkspaceId: w.ID, WorkspaceName: w.Name, AdminEmail: admin.Email}
	projects, err := s.q.Projects(ctx, c.workspace)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if !c.subject.Can(authz.EnvUse, p.Name) {
			continue
		}
		v, err := s.q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: p.Version})
		if err != nil {
			return nil, err
		}
		dp := &syncv1.DesiredProject{Id: p.ID, Name: p.Name, Version: p.Version, ConfigYaml: v.Config}
		if c.subject.Can(authz.SecretsRead, p.Name) {
			secrets, err := s.q.ProjectSecrets(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			for _, sec := range secrets {
				if dp.Secrets == nil {
					dp.Secrets = map[string][]byte{}
				}
				dp.Secrets[sec.Name] = sec.Ciphertext
			}
		}
		out.Projects = append(out.Projects, dp)
	}
	gen, wrapped, pending, err := s.keyFor(ctx, c, device)
	if err != nil {
		return nil, err
	}
	if gen > 0 {
		out.Key = &syncv1.SecretsKey{Generation: gen, Wrapped: wrapped}
	}
	for _, d := range pending {
		out.PendingDevices = append(out.PendingDevices, &syncv1.PendingDevice{DeviceId: d.DeviceID, UserId: d.UserID, UserEmail: d.Email, DeviceName: d.Name, PublicKey: d.PublicKey})
	}
	return out, nil
}

func fingerprint(m proto.Message) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (x *syncService) Watch(ctx context.Context, req *connect.Request[syncv1.WatchRequest], stream *connect.ServerStream[syncv1.WatchResponse]) error {
	s, c := x.s, callerOf(ctx)
	if err := s.seeDevice(ctx, c, req.Msg.Device); err != nil {
		return err
	}
	// The token ends the stream: the agent comes back with a fresh one.
	ctx, cancel := context.WithDeadline(ctx, c.expires)
	defer cancel()
	wake, stop := s.hub.subscribe(c.workspace)
	defer stop()
	tick := time.NewTicker(recheck)
	defer tick.Stop()
	last := ""
	for {
		// Membership and roles are read again every time: removing a member cuts
		// their devices off here.
		if err := s.load(ctx, c, c.workspace); err != nil {
			return err
		}
		ds, err := s.desired(ctx, c, req.Msg.Device.GetId())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return internal(err)
		}
		if fp := fingerprint(ds); fp != last {
			if err := stream.Send(ds); err != nil {
				return err
			}
			last = fp
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-tick.C:
		}
	}
}

// seeDevice records that a device of the caller is connected.
func (s *Service) seeDevice(ctx context.Context, c *caller, d *syncv1.Device) error {
	if d.GetId() == "" {
		return invalid("the device id is missing")
	}
	err := s.q.UpsertDevice(ctx, store.UpsertDeviceParams{WorkspaceID: c.workspace, DeviceID: d.Id, UserID: c.userID,
		Name: d.Name, AgentVersion: d.AgentVersion, LastSeen: s.now()})
	if err != nil {
		return internal(err)
	}
	if d.PublicKey == "" {
		return nil
	}
	return s.tx(ctx, func(q *store.Queries) error {
		return s.saveDeviceKey(ctx, q, c.workspace, c.userID, d.Id, d.Name, d.PublicKey)
	})
}

func (x *syncService) ReportStatus(ctx context.Context, req *connect.Request[syncv1.ReportStatusRequest]) (*connect.Response[syncv1.ReportStatusResponse], error) {
	s, c := x.s, callerOf(ctx)
	d := req.Msg.Device
	if err := s.seeDevice(ctx, c, d); err != nil {
		return nil, err
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.ClearDeviceEnvironments(ctx, store.ClearDeviceEnvironmentsParams{WorkspaceID: c.workspace, DeviceID: d.Id, UserID: c.userID}); err != nil {
			return err
		}
		for _, e := range req.Msg.Environments {
			if err := q.AddDeviceEnvironment(ctx, store.AddDeviceEnvironmentParams{
				WorkspaceID: c.workspace, DeviceID: d.Id, UserID: c.userID, EnvID: e.Id, Project: e.Project, Kind: e.Kind,
				Name: e.Name, Branch: e.Branch, Phase: e.Phase, Url: e.Url, Error: e.Error, ConfigVersion: e.ConfigVersion,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&syncv1.ReportStatusResponse{}), nil
}

func (x *syncService) ListDevices(ctx context.Context, req *connect.Request[syncv1.ListDevicesRequest]) (*connect.Response[syncv1.ListDevicesResponse], error) {
	s, c := x.s, callerOf(ctx)
	devices, err := s.q.Devices(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	envs, err := s.q.DeviceEnvironments(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	byDevice := map[[2]string][]*syncv1.EnvironmentReport{}
	for _, e := range envs {
		k := [2]string{e.DeviceID, e.UserID}
		byDevice[k] = append(byDevice[k], &syncv1.EnvironmentReport{Project: e.Project, Id: e.EnvID, Kind: e.Kind, Name: e.Name,
			Branch: e.Branch, Phase: e.Phase, Url: e.Url, Error: e.Error, ConfigVersion: e.ConfigVersion})
	}
	resp := &syncv1.ListDevicesResponse{}
	now := s.now()
	for _, d := range devices {
		var shown []*syncv1.EnvironmentReport
		for _, e := range byDevice[[2]string{d.DeviceID, d.UserID}] {
			// Others' devices show the projects the caller manages.
			if d.UserID != c.userID && !c.subject.Can(authz.MembersManage, e.Project) {
				continue
			}
			if req.Msg.Project != "" && e.Project != req.Msg.Project {
				continue
			}
			shown = append(shown, e)
		}
		if d.UserID != c.userID && len(shown) == 0 {
			continue
		}
		if req.Msg.Project != "" && len(shown) == 0 {
			continue
		}
		resp.Devices = append(resp.Devices, &syncv1.DeviceStatus{
			Device:    &syncv1.Device{Id: d.DeviceID, Name: d.Name, AgentVersion: d.AgentVersion},
			UserEmail: d.Email, LastSeen: timestamppb.New(d.LastSeen), Online: now.Sub(d.LastSeen) < onlineFor,
			Environments: shown,
		})
	}
	return connect.NewResponse(resp), nil
}

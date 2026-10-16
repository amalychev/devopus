package server

import (
	"context"
	"regexp"

	"connectrpc.com/connect"
	"filippo.io/age"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
)

// Secrets are end-to-end (ADR-0009, internal/secrets): the server keeps the workspace's
// public key, its private key wrapped for devices, and ciphertexts. It checks who may
// get which ciphertext; reading a value takes the key, which only devices hold.

var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func checkPublicKey(k string) error {
	if _, err := age.ParseX25519Recipient(k); err != nil {
		return invalid("public key: not an age X25519 key (age1…)")
	}
	return nil
}

// saveDeviceKey records a device's public key; a new key voids what was wrapped for
// the old one.
func (s *Service) saveDeviceKey(ctx context.Context, q *store.Queries, ws, user, device, name, key string) error {
	if err := checkPublicKey(key); err != nil {
		return err
	}
	old, err := q.DeviceKey(ctx, store.DeviceKeyParams{WorkspaceID: ws, DeviceID: device, UserID: user})
	switch {
	case err == nil && old.PublicKey == key:
		return nil
	case err == nil:
		if err := q.DeleteDeviceWraps(ctx, store.DeleteDeviceWrapsParams{WorkspaceID: ws, DeviceID: device, UserID: user}); err != nil {
			return internal(err)
		}
	case !noRows(err):
		return internal(err)
	}
	if err := q.UpsertDeviceKey(ctx, store.UpsertDeviceKeyParams{WorkspaceID: ws, DeviceID: device, UserID: user, Name: name, PublicKey: key, CreatedAt: s.now()}); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) GetSecretsKey(ctx context.Context, req *connect.Request[workspacev1.GetSecretsKeyRequest]) (*connect.Response[workspacev1.GetSecretsKeyResponse], error) {
	c := callerOf(ctx)
	k, err := s.q.WorkspaceKey(ctx, c.workspace)
	if noRows(err) {
		return connect.NewResponse(&workspacev1.GetSecretsKeyResponse{}), nil
	}
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.GetSecretsKeyResponse{Generation: k.Generation, PublicKey: k.PublicKey}
	if req.Msg.DeviceId != "" {
		w, err := s.q.KeyWrap(ctx, store.KeyWrapParams{WorkspaceID: c.workspace, Generation: k.Generation, DeviceID: req.Msg.DeviceId, UserID: c.userID})
		if err == nil {
			resp.Wrapped = w.Wrapped
		} else if !noRows(err) {
			return nil, internal(err)
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RegisterDeviceKey(ctx context.Context, req *connect.Request[workspacev1.RegisterDeviceKeyRequest]) (*connect.Response[workspacev1.RegisterDeviceKeyResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if m.DeviceId == "" {
		return nil, invalid("the device id is missing")
	}
	if err := s.tx(ctx, func(q *store.Queries) error {
		return s.saveDeviceKey(ctx, q, c.workspace, c.userID, m.DeviceId, m.DeviceName, m.PublicKey)
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RegisterDeviceKeyResponse{}), nil
}

func (s *Service) CreateSecretsKey(ctx context.Context, req *connect.Request[workspacev1.CreateSecretsKeyRequest]) (*connect.Response[workspacev1.CreateSecretsKeyResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if err := checkPublicKey(m.PublicKey); err != nil {
		return nil, err
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if w.KeyGeneration != 0 {
			return connect.NewError(connect.CodeAlreadyExists, errorf("the workspace has a key already"))
		}
		if _, err := q.DeviceKey(ctx, store.DeviceKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID}); err != nil {
			return invalid("register the device's key first")
		}
		now := s.now()
		if err := q.AddWorkspaceKey(ctx, store.AddWorkspaceKeyParams{WorkspaceID: c.workspace, Generation: 1, PublicKey: m.PublicKey, CreatedBy: c.userID, CreatedAt: now}); err != nil {
			return internal(err)
		}
		if err := q.SetKeyGeneration(ctx, store.SetKeyGenerationParams{ID: c.workspace, KeyGeneration: 1}); err != nil {
			return internal(err)
		}
		if err := q.AddKeyWrap(ctx, store.AddKeyWrapParams{WorkspaceID: c.workspace, Generation: 1, DeviceID: m.DeviceId, UserID: c.userID,
			Wrapped: m.Wrapped, WrappedBy: c.userID, CreatedAt: now}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "secrets.key_created", "generation 1", "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CreateSecretsKeyResponse{Generation: 1}), nil
}

func (s *Service) ProvideKeyWraps(ctx context.Context, req *connect.Request[workspacev1.ProvideKeyWrapsRequest]) (*connect.Response[workspacev1.ProvideKeyWrapsResponse], error) {
	c, m := callerOf(ctx), req.Msg
	delivered := 0
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if m.Generation != w.KeyGeneration {
			return connect.NewError(connect.CodeAborted, errorf("the key was rotated: generation %d is current", w.KeyGeneration))
		}
		for _, wr := range m.Wraps {
			// Only devices of members, which registered their keys here.
			if _, err := q.DeviceKey(ctx, store.DeviceKeyParams{WorkspaceID: c.workspace, DeviceID: wr.DeviceId, UserID: wr.UserId}); err != nil {
				continue
			}
			if err := q.AddKeyWrap(ctx, store.AddKeyWrapParams{WorkspaceID: c.workspace, Generation: m.Generation, DeviceID: wr.DeviceId, UserID: wr.UserId,
				Wrapped: wr.Wrapped, WrappedBy: c.userID, CreatedAt: s.now()}); err != nil {
				return internal(err)
			}
			delivered++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.ProvideKeyWrapsResponse{Delivered: int32(delivered)}), nil
}

func (s *Service) PrepareRotation(ctx context.Context, _ *connect.Request[workspacev1.PrepareRotationRequest]) (*connect.Response[workspacev1.PrepareRotationResponse], error) {
	c := callerOf(ctx)
	w, err := s.q.Workspace(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	all, err := s.q.WorkspaceSecrets(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	devices, err := s.q.WorkspaceDeviceKeys(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	members, err := s.q.Members(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	emails := map[string]string{}
	for _, m := range members {
		emails[m.UserID] = m.Email
	}
	resp := &workspacev1.PrepareRotationResponse{Generation: w.KeyGeneration}
	for _, sec := range all {
		resp.Secrets = append(resp.Secrets, &workspacev1.SealedSecret{Project: sec.Project, Name: sec.Name, Ciphertext: sec.Ciphertext})
	}
	for _, d := range devices {
		resp.Devices = append(resp.Devices, &workspacev1.DeviceKey{DeviceId: d.DeviceID, UserId: d.UserID, UserEmail: emails[d.UserID], DeviceName: d.Name, PublicKey: d.PublicKey})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RotateSecretsKey(ctx context.Context, req *connect.Request[workspacev1.RotateSecretsKeyRequest]) (*connect.Response[workspacev1.RotateSecretsKeyResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if err := checkPublicKey(m.PublicKey); err != nil {
		return nil, err
	}
	var gen int32
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if w.KeyGeneration == 0 || m.FromGeneration != w.KeyGeneration {
			return connect.NewError(connect.CodeAborted, errorf("generation %d is current; look again and retry", w.KeyGeneration))
		}
		all, err := q.WorkspaceSecrets(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		sent := map[[2]string][]byte{}
		for _, sec := range m.Secrets {
			sent[[2]string{sec.Project, sec.Name}] = sec.Ciphertext
		}
		gen = w.KeyGeneration + 1
		now := s.now()
		for _, old := range all {
			ct, ok := sent[[2]string{old.Project, old.Name}]
			if !ok {
				return connect.NewError(connect.CodeFailedPrecondition, errorf("secret %s of %s is missing from the rotation; look again and retry", old.Name, old.Project))
			}
			if err := q.SetSecret(ctx, store.SetSecretParams{WorkspaceID: c.workspace, ProjectID: old.ProjectID, Name: old.Name, Generation: gen,
				Ciphertext: ct, UpdatedBy: c.userID, UpdatedAt: now}); err != nil {
				return internal(err)
			}
		}
		if err := q.AddWorkspaceKey(ctx, store.AddWorkspaceKeyParams{WorkspaceID: c.workspace, Generation: gen, PublicKey: m.PublicKey, CreatedBy: c.userID, CreatedAt: now}); err != nil {
			return internal(err)
		}
		if err := q.SetKeyGeneration(ctx, store.SetKeyGenerationParams{ID: c.workspace, KeyGeneration: gen}); err != nil {
			return internal(err)
		}
		for _, wr := range m.Wraps {
			if _, err := q.DeviceKey(ctx, store.DeviceKeyParams{WorkspaceID: c.workspace, DeviceID: wr.DeviceId, UserID: wr.UserId}); err != nil {
				continue
			}
			if err := q.AddKeyWrap(ctx, store.AddKeyWrapParams{WorkspaceID: c.workspace, Generation: gen, DeviceID: wr.DeviceId, UserID: wr.UserId,
				Wrapped: wr.Wrapped, WrappedBy: c.userID, CreatedAt: now}); err != nil {
				return internal(err)
			}
		}
		if err := q.DeleteOldWraps(ctx, store.DeleteOldWrapsParams{WorkspaceID: c.workspace, Generation: gen}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "secrets.key_rotated", errorf("generation %d", gen).Error(), "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RotateSecretsKeyResponse{Generation: gen}), nil
}

func (s *Service) SetSecret(ctx context.Context, req *connect.Request[workspacev1.SetSecretRequest]) (*connect.Response[workspacev1.SetSecretResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if !secretNameRe.MatchString(m.Name) {
		return nil, invalid("secret name %q: letters, digits and _, as in ${{ secrets.NAME }}", m.Name)
	}
	if len(m.Ciphertext) == 0 || len(m.Ciphertext) > 64<<10 {
		return nil, invalid("the encrypted value is empty or larger than 64 KiB")
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.Workspace(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if w.KeyGeneration == 0 || m.Generation != w.KeyGeneration {
			return connect.NewError(connect.CodeAborted, errorf("the value is encrypted to key generation %d, generation %d is current; retry", m.Generation, w.KeyGeneration))
		}
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		if err := q.SetSecret(ctx, store.SetSecretParams{WorkspaceID: c.workspace, ProjectID: p.ID, Name: m.Name, Generation: m.Generation,
			Ciphertext: m.Ciphertext, UpdatedBy: c.userID, UpdatedAt: s.now()}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "secret.set", m.Name, m.Project)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.SetSecretResponse{}), nil
}

func (s *Service) ListSecrets(ctx context.Context, req *connect.Request[workspacev1.ListSecretsRequest]) (*connect.Response[workspacev1.ListSecretsResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ProjectSecrets(ctx, p.ID)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListSecretsResponse{}
	for _, r := range rows {
		resp.Secrets = append(resp.Secrets, &workspacev1.SecretInfo{Name: r.Name, UpdatedBy: r.UpdatedByEmail, UpdatedAt: timestamppb.New(r.UpdatedAt)})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RemoveSecret(ctx context.Context, req *connect.Request[workspacev1.RemoveSecretRequest]) (*connect.Response[workspacev1.RemoveSecretResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		n, err := q.DeleteSecret(ctx, store.DeleteSecretParams{ProjectID: p.ID, Name: m.Name})
		if err != nil {
			return internal(err)
		}
		if n == 0 {
			return notFound("%s has no secret %s", m.Project, m.Name)
		}
		return s.audit(ctx, q, c.workspace, "secret.removed", m.Name, m.Project)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveSecretResponse{}), nil
}

// keyFor is what the sync stream gives a device about the key, and to key holders
// the devices waiting for it.
func (s *Service) keyFor(ctx context.Context, c *caller, device string) (int32, []byte, []store.PendingDevicesRow, error) {
	k, err := s.q.WorkspaceKey(ctx, c.workspace)
	if noRows(err) {
		return 0, nil, nil, nil
	}
	if err != nil {
		return 0, nil, nil, err
	}
	var wrapped []byte
	w, err := s.q.KeyWrap(ctx, store.KeyWrapParams{WorkspaceID: c.workspace, Generation: k.Generation, DeviceID: device, UserID: c.userID})
	if err == nil {
		wrapped = w.Wrapped
	} else if !noRows(err) {
		return 0, nil, nil, err
	}
	var pending []store.PendingDevicesRow
	if wrapped != nil && (c.subject.Role == authz.Admin || c.subject.Role == authz.Teamlead) {
		if pending, err = s.q.PendingDevices(ctx, store.PendingDevicesParams{WorkspaceID: c.workspace, Generation: k.Generation}); err != nil {
			return 0, nil, nil, err
		}
	}
	return k.Generation, wrapped, pending, nil
}

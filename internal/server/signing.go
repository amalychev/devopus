package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/signing"
)

// Signatures of settings (ADR-0018). The server checks what it stores — a wrong
// certificate or signature is a mistake to report at once — but agents trust none of
// it: they check everything again against the root they pinned.

func (s *Service) RegisterSigningKey(ctx context.Context, req *connect.Request[workspacev1.RegisterSigningKeyRequest]) (*connect.Response[workspacev1.RegisterSigningKeyResponse], error) {
	c, m := callerOf(ctx), req.Msg
	if m.DeviceId == "" || len(m.PublicKey) != ed25519.PublicKeySize {
		return nil, invalid("a device id and an Ed25519 public key are needed")
	}
	var certified bool
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.UpsertSigningKey(ctx, store.UpsertSigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID,
			Name: m.DeviceName, PublicKey: m.PublicKey, CreatedAt: s.now()}); err != nil {
			return internal(err)
		}
		k, err := q.SigningKey(ctx, store.SigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID})
		if err != nil {
			return internal(err)
		}
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		certified = len(k.Certificate) > 0 || (w.TrustRootDevice == m.DeviceId && w.TrustRootUser == c.userID && bytes.Equal(w.TrustRoot, m.PublicKey))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RegisterSigningKeyResponse{Certified: certified}), nil
}

func (s *Service) SetTrustRoot(ctx context.Context, req *connect.Request[workspacev1.SetTrustRootRequest]) (*connect.Response[workspacev1.SetTrustRootResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if len(w.TrustRoot) > 0 {
			return connect.NewError(connect.CodeAlreadyExists, errorf("the workspace has a root of trust already: agents pinned it"))
		}
		k, err := q.SigningKey(ctx, store.SigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID})
		if noRows(err) {
			return invalid("the device %q has no signing key registered", m.DeviceId)
		}
		if err != nil {
			return internal(err)
		}
		if err := q.SetTrustRoot(ctx, store.SetTrustRootParams{ID: c.workspace, TrustRoot: k.PublicKey, TrustRootDevice: k.DeviceID, TrustRootUser: c.userID}); err != nil {
			return internal(err)
		}
		if err := s.audit(ctx, q, c.workspace, "trust.root", k.Name, signing.Fingerprint(k.PublicKey)); err != nil {
			return internal(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.SetTrustRootResponse{}), nil
}

func (s *Service) ListSigningKeys(ctx context.Context, req *connect.Request[workspacev1.ListSigningKeysRequest]) (*connect.Response[workspacev1.ListSigningKeysResponse], error) {
	c := callerOf(ctx)
	w, keys, err := s.signingKeys(ctx, s.q, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListSigningKeysResponse{}
	for _, k := range keys {
		if len(w.TrustRoot) > 0 && k.DeviceID == w.TrustRootDevice && k.UserID == w.TrustRootUser {
			resp.Root = &workspacev1.TrustRoot{PublicKey: w.TrustRoot, DeviceId: k.DeviceID, DeviceName: k.Name, UserEmail: k.Email}
		}
		resp.Keys = append(resp.Keys, &workspacev1.SigningKey{DeviceId: k.DeviceID, DeviceName: k.Name, UserId: k.UserID, UserEmail: k.Email,
			Role: k.Role, PublicKey: k.PublicKey, Certificate: k.Certificate, CreatedAt: timestamppb.New(k.CreatedAt)})
	}
	if resp.Root == nil && len(w.TrustRoot) > 0 {
		resp.Root = &workspacev1.TrustRoot{PublicKey: w.TrustRoot, DeviceId: w.TrustRootDevice}
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) signingKeys(ctx context.Context, q *store.Queries, ws string) (store.Workspace, []store.SigningKeysRow, error) {
	w, err := q.Workspace(ctx, ws)
	if err != nil {
		return w, nil, err
	}
	keys, err := q.SigningKeys(ctx, ws)
	return w, keys, err
}

func (s *Service) CertifySigningKey(ctx context.Context, req *connect.Request[workspacev1.CertifySigningKeyRequest]) (*connect.Response[workspacev1.CertifySigningKeyResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		w, err := q.WorkspaceForUpdate(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		if len(w.TrustRoot) == 0 {
			return connect.NewError(connect.CodeFailedPrecondition, errorf("the workspace has no root of trust yet"))
		}
		keys, err := q.SigningKeys(ctx, c.workspace)
		if err != nil {
			return internal(err)
		}
		i := slices.IndexFunc(keys, func(k store.SigningKeysRow) bool { return k.DeviceID == m.DeviceId && k.UserID == m.UserId })
		if i < 0 {
			return notFound("no signing key of device %q", m.DeviceId)
		}
		k, u := keys[i], keys[i]
		set := signing.Set{Workspace: c.workspace, Root: w.TrustRoot}
		if !set.Certified(signing.Signer{DeviceID: k.DeviceID, UserID: k.UserID, Email: u.Email, Public: k.PublicKey, Certificate: m.Certificate}) {
			return invalid("the certificate is not the root's for this key")
		}
		if err := q.CertifySigningKey(ctx, store.CertifySigningKeyParams{WorkspaceID: c.workspace, DeviceID: k.DeviceID, UserID: k.UserID, Certificate: m.Certificate}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "trust.certify", u.Email, k.Name+" "+signing.Fingerprint(k.PublicKey))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CertifySigningKeyResponse{}), nil
}

func (s *Service) SignVersion(ctx context.Context, req *connect.Request[workspacev1.SignVersionRequest]) (*connect.Response[workspacev1.SignVersionResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		p, err := q.ProjectByNameForUpdate(ctx, store.ProjectByNameForUpdateParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		v, err := q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: m.Version})
		if noRows(err) {
			return notFound("%s has no version %d", m.Project, m.Version)
		}
		if err != nil {
			return internal(err)
		}
		if v.AuthorID != c.userID {
			return denied("only the author of a version signs it")
		}
		k, err := q.SigningKey(ctx, store.SigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID})
		if noRows(err) {
			return invalid("the device %q has no signing key registered", m.DeviceId)
		}
		if err != nil {
			return internal(err)
		}
		if !ed25519.Verify(k.PublicKey, signing.SettingsMessage(c.workspace, m.Project, m.Version, []byte(v.Config)), m.Signature) {
			return invalid("the signature does not match version %d of %s", m.Version, m.Project)
		}
		return q.SignProjectVersion(ctx, store.SignProjectVersionParams{ProjectID: p.ID, Version: m.Version, Signature: m.Signature, SignerDevice: m.DeviceId})
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.SignVersionResponse{}), nil
}

// trustFor is what the server gives agents to check signatures with.
func (s *Service) trustFor(ctx context.Context, ws string) (*syncv1.Trust, error) {
	w, keys, err := s.signingKeys(ctx, s.q, ws)
	if err != nil || len(w.TrustRoot) == 0 {
		return nil, err
	}
	t := &syncv1.Trust{Root: w.TrustRoot, RootDevice: w.TrustRootDevice}
	for _, k := range keys {
		if k.DeviceID == w.TrustRootDevice && k.UserID == w.TrustRootUser {
			t.RootEmail = k.Email
		}
		if len(k.Certificate) > 0 {
			t.Signers = append(t.Signers, &syncv1.Signer{DeviceId: k.DeviceID, UserId: k.UserID, Email: k.Email, PublicKey: k.PublicKey, Certificate: k.Certificate})
		}
	}
	return t, nil
}

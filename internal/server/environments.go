package server

import (
	"context"
	"fmt"
	"crypto/ed25519"
	"net"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/signing"
)

// Environments of projects on servers of the admin (R14, ADR-0020): a server runs a
// project as staging or production at a public domain.

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// publicHost checks a domain a server's environment answers on: a DNS name of the
// internet, not a local one, not an address.
func publicHost(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	switch {
	case d == "":
		return "", invalid("the environment needs its public domain: --domain staging.shop.example.com")
	case net.ParseIP(d) != nil:
		return "", invalid("%s is an address: give a domain name, Let's Encrypt certifies names", d)
	case !hostRe.MatchString(d):
		return "", invalid("%q is not a domain name", d)
	case strings.HasSuffix(d, ".localhost") || strings.HasSuffix(d, ".local") || strings.HasSuffix(d, ".test") || strings.HasSuffix(d, ".internal"):
		return "", invalid("%s is not a public domain: Let's Encrypt certifies names of the internet", d)
	}
	return d, nil
}

func (s *Service) CreateEnvironment(ctx context.Context, req *connect.Request[workspacev1.CreateEnvironmentRequest]) (*connect.Response[workspacev1.CreateEnvironmentResponse], error) {
	c, m := callerOf(ctx), req.Msg
	switch m.Kind {
	case "staging":
	case "production":
		// Production runs what someone with production.deploy chooses: they make it too.
		if !c.subject.Can(authz.ProductionDeploy, m.Project) {
			return nil, denied("a production environment needs the permission %s in project %q; the admin grants it", authz.ProductionDeploy, m.Project)
		}
	default:
		return nil, invalid("the kind is staging or production, not %q", m.Kind)
	}
	domain, err := publicHost(m.Domain)
	if err != nil {
		return nil, err
	}
	var env *workspacev1.Environment
	err = s.tx(ctx, func(q *store.Queries) error {
		sv, err := s.serverNamed(ctx, q, c, m.Server)
		if err != nil {
			return err
		}
		// The server's other projects must be the caller's too: a project's teamlead
		// does not put their code on a server another project runs on.
		if err := canManageServer(c.subject, sortedSet(append(append([]string{}, sv.Projects...), m.Project))); err != nil {
			return err
		}
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		taken, err := q.DomainTaken(ctx, store.DomainTakenParams{Domain: domain, WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID})
		if err != nil {
			return internal(err)
		}
		if taken {
			return connect.NewError(connect.CodeAlreadyExists, errorf("%s is the domain of another environment", domain))
		}
		k, err := q.SigningKey(ctx, store.SigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID})
		if noRows(err) {
			return invalid("the environment must be signed by a device of yours with a registered signing key: create it with devopus")
		}
		if err != nil {
			return internal(err)
		}
		if !ed25519.Verify(k.PublicKey, signing.EnvironmentMessage(c.workspace, sv.ID, m.Project, m.Kind, domain), m.Signature) {
			return invalid("the signature does not match the environment")
		}
		if err := q.SetProjectRole(ctx, store.SetProjectRoleParams{WorkspaceID: c.workspace, ProjectID: p.ID, UserID: sv.ID, Role: string(authz.Server)}); err != nil {
			return internal(err)
		}
		now := s.now()
		if err := q.UpsertServerEnvironment(ctx, store.UpsertServerEnvironmentParams{WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID,
			Kind: m.Kind, Domain: domain, Signature: m.Signature, SignerDevice: m.DeviceId, CreatedBy: c.userID, CreatedAt: now}); err != nil {
			return internal(err)
		}
		env = &workspacev1.Environment{Project: m.Project, Server: m.Server, Kind: m.Kind, Domain: domain, CreatedBy: c.email, CreatedAt: timestamppb.New(now)}
		return s.audit(ctx, q, c.workspace, "environment.created", m.Project, m.Kind+" on "+m.Server+" at "+domain)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CreateEnvironmentResponse{Environment: env}), nil
}

func (s *Service) ListEnvironments(ctx context.Context, req *connect.Request[workspacev1.ListEnvironmentsRequest]) (*connect.Response[workspacev1.ListEnvironmentsResponse], error) {
	c := callerOf(ctx)
	rows, err := s.q.ServerEnvironments(ctx, c.workspace)
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListEnvironmentsResponse{}
	for _, r := range rows {
		if !c.subject.Can(authz.WorkspaceView, r.Project) {
			continue
		}
		e := &workspacev1.Environment{Project: r.Project, Server: r.Server, Kind: r.Kind,
			Domain: r.Domain, CreatedBy: r.Creator, CreatedAt: timestamppb.New(r.CreatedAt)}
		if r.Kind == "production" {
			d, err := s.q.LastDeploy(ctx, store.LastDeployParams{WorkspaceID: c.workspace, ServerID: r.ServerID, ProjectID: r.ProjectID})
			if err != nil && !noRows(err) {
				return nil, internal(err)
			}
			if err == nil {
				e.DeployedVersion, e.DeployedBy, e.DeployedAt = d.Version, d.Deployer, timestamppb.New(d.DeployedAt)
			}
		}
		resp.Environments = append(resp.Environments, e)
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) RemoveEnvironment(ctx context.Context, req *connect.Request[workspacev1.RemoveEnvironmentRequest]) (*connect.Response[workspacev1.RemoveEnvironmentResponse], error) {
	c, m := callerOf(ctx), req.Msg
	err := s.tx(ctx, func(q *store.Queries) error {
		sv, err := s.serverNamed(ctx, q, c, m.Server)
		if err != nil {
			return err
		}
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		if _, err := q.ServerEnvironment(ctx, store.ServerEnvironmentParams{WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID}); noRows(err) {
			return notFound("%s has no environment on %s", m.Project, m.Server)
		} else if err != nil {
			return internal(err)
		}
		if err := q.DeleteServerEnvironment(ctx, store.DeleteServerEnvironmentParams{WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID}); err != nil {
			return internal(err)
		}
		// The server stops running the project.
		if err := q.DeleteProjectRole(ctx, store.DeleteProjectRoleParams{ProjectID: p.ID, UserID: sv.ID}); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "environment.removed", m.Project, "on "+m.Server)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveEnvironmentResponse{}), nil
}

func (s *Service) DeployProduction(ctx context.Context, req *connect.Request[workspacev1.DeployProductionRequest]) (*connect.Response[workspacev1.DeployProductionResponse], error) {
	c, m := callerOf(ctx), req.Msg
	var env *workspacev1.Environment
	err := s.tx(ctx, func(q *store.Queries) error {
		sv, err := s.serverNamed(ctx, q, c, m.Server)
		if err != nil {
			return err
		}
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: m.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", m.Project)
		}
		if err != nil {
			return internal(err)
		}
		e, err := q.ServerEnvironment(ctx, store.ServerEnvironmentParams{WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID})
		if noRows(err) || (err == nil && e.Kind != "production") {
			return connect.NewError(connect.CodeFailedPrecondition, errorf("%s has no production environment on %s: 'devopus env create production'", m.Project, m.Server))
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
		k, err := q.SigningKey(ctx, store.SigningKeyParams{WorkspaceID: c.workspace, DeviceID: m.DeviceId, UserID: c.userID})
		if noRows(err) {
			return invalid("a deploy must be signed by a device of yours with a registered signing key: deploy with devopus")
		}
		if err != nil {
			return internal(err)
		}
		if !ed25519.Verify(k.PublicKey, signing.DeployMessage(c.workspace, sv.ID, m.Project, m.Version, []byte(v.Config)), m.Signature) {
			return invalid("the signature does not match version %d of %s", m.Version, m.Project)
		}
		now := s.now()
		if err := q.AddDeploy(ctx, store.AddDeployParams{WorkspaceID: c.workspace, ServerID: sv.ID, ProjectID: p.ID, Version: m.Version,
			Signature: m.Signature, SignerDevice: m.DeviceId, DeployedBy: c.userID, DeployedAt: now}); err != nil {
			return internal(err)
		}
		env = &workspacev1.Environment{Project: m.Project, Server: m.Server, Kind: e.Kind, Domain: e.Domain,
			DeployedVersion: m.Version, DeployedBy: c.email, DeployedAt: timestamppb.New(now)}
		return s.audit(ctx, q, c.workspace, "production.deployed", m.Project, fmt.Sprintf("v%d on %s", m.Version, m.Server))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.DeployProductionResponse{Environment: env}), nil
}

// serverEnvironment is how a server runs a project, for its desired state.
func (s *Service) serverEnvironment(ctx context.Context, c *caller, projectID string) (*syncv1.ServerEnvironment, error) {
	if c.server == "" {
		return nil, nil
	}
	e, err := s.q.ServerEnvironment(ctx, store.ServerEnvironmentParams{WorkspaceID: c.workspace, ServerID: c.userID, ProjectID: projectID})
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &syncv1.ServerEnvironment{Kind: e.Kind, Domain: e.Domain, Signature: e.Signature, SignerDevice: e.SignerDevice}, nil
}

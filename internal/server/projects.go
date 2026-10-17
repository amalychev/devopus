package server

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/catalog"
	"github.com/amalychev/devopus/internal/server/store"
	"github.com/amalychev/devopus/internal/spec"
)

// A project's settings are a spec.Project kept in versions that never change. Every
// change reads the current version under a lock on the project, applies itself,
// passes spec's Check and becomes the next version; a rollback is a change too.

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func checkDomain(d string) error {
	if !domainRe.MatchString(d) {
		return invalid("domain %q: a host name such as shop.localhost", d)
	}
	return nil
}

func configInfo(v store.ProjectVersionRow) *workspacev1.ProjectConfig {
	return &workspacev1.ProjectConfig{Version: v.Version, Yaml: v.Config, Author: v.Author, Message: v.Message, CreatedAt: timestamppb.New(v.CreatedAt),
		Signature: v.Signature, SignerDevice: v.SignerDevice}
}

func projectInfo(p store.Project, role authz.Role) *workspacev1.Project {
	return &workspacev1.Project{Id: p.ID, Name: p.Name, CreatedAt: timestamppb.New(p.CreatedAt), Role: string(role), Version: p.Version}
}

// addVersion stores a project's next version and makes it current.
func (s *Service) addVersion(ctx context.Context, q *store.Queries, p store.Project, cfg *spec.Project, message string) (*workspacev1.ProjectConfig, error) {
	if err := cfg.Check(); err != nil {
		return nil, invalid("%v", err)
	}
	b, err := cfg.YAML()
	if err != nil {
		return nil, internal(err)
	}
	c := callerOf(ctx)
	v := p.Version + 1
	if err := q.AddProjectVersion(ctx, store.AddProjectVersionParams{
		WorkspaceID: p.WorkspaceID, ProjectID: p.ID, Version: v, Config: string(b), AuthorID: c.userID, Message: message, CreatedAt: s.now(),
	}); err != nil {
		return nil, internal(err)
	}
	if err := q.SetProjectVersion(ctx, store.SetProjectVersionParams{ID: p.ID, Version: v}); err != nil {
		return nil, internal(err)
	}
	if err := s.audit(ctx, q, p.WorkspaceID, "project.changed", p.Name, fmt.Sprintf("v%d: %s", v, message)); err != nil {
		return nil, internal(err)
	}
	row, err := q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: v})
	if err != nil {
		return nil, internal(err)
	}
	return configInfo(row), nil
}

// change applies fn to the current settings of a project and stores the result as a
// new version. base is the version the caller saw; 0 skips the check. A change that
// changes nothing adds no version.
func (s *Service) change(ctx context.Context, project string, base int32, fn func(cfg *spec.Project) (string, error)) (*workspacev1.ProjectConfig, error) {
	c := callerOf(ctx)
	var out *workspacev1.ProjectConfig
	err := s.tx(ctx, func(q *store.Queries) error {
		p, err := q.ProjectByNameForUpdate(ctx, store.ProjectByNameForUpdateParams{WorkspaceID: c.workspace, Name: project})
		if noRows(err) {
			return notFound("no project %q in this workspace", project)
		}
		if err != nil {
			return internal(err)
		}
		if base != 0 && base != p.Version {
			return connect.NewError(connect.CodeAborted, errorf("the settings of %s changed since version %d (now %d); look again and retry", project, base, p.Version))
		}
		cur, err := q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: p.Version})
		if err != nil {
			return internal(err)
		}
		cfg, err := spec.ParseYAML([]byte(cur.Config))
		if err != nil {
			return internal(err)
		}
		message, err := fn(cfg)
		if err != nil {
			return err
		}
		cfg.Name = p.Name
		if b, err := cfg.YAML(); err == nil && string(b) == cur.Config {
			out = configInfo(cur)
			return nil
		}
		out, err = s.addVersion(ctx, q, p, cfg, message)
		return err
	})
	return out, err
}

func (s *Service) ListServiceTypes(context.Context, *connect.Request[workspacev1.ListServiceTypesRequest]) (*connect.Response[workspacev1.ListServiceTypesResponse], error) {
	resp := &workspacev1.ListServiceTypesResponse{}
	for _, t := range catalog.Types() {
		resp.Types = append(resp.Types, &workspacev1.ServiceType{Name: t.Name, Kind: string(t.Kind), Repo: t.Repo,
			Port: int32(t.Port), UiPort: int32(t.UIPort), Aliases: t.Aliases})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) CreateProject(ctx context.Context, req *connect.Request[workspacev1.CreateProjectRequest]) (*connect.Response[workspacev1.CreateProjectResponse], error) {
	m := req.Msg
	name := strings.TrimSpace(m.Name)
	if !spec.NameRe.MatchString(name) {
		return nil, invalid("project name %q: lowercase letters, digits and dashes, starting with a letter, up to 40", name)
	}
	cfg := &spec.Project{Apps: map[string]*spec.App{}, Services: map[string]*spec.Service{}}
	message := "created"
	if strings.TrimSpace(m.ConfigYaml) != "" {
		var err error
		if cfg, err = spec.ParseYAML([]byte(m.ConfigYaml)); err != nil {
			return nil, invalid("%v", err)
		}
		message = "created from an import"
	}
	cfg.Name = name
	switch {
	case m.Domain != "":
		cfg.Domain = m.Domain
	case cfg.Domain == "":
		cfg.Domain = name + ".localhost"
	}
	if err := checkDomain(cfg.Domain); err != nil {
		return nil, err
	}
	c := callerOf(ctx)
	var (
		p    store.Project
		role authz.Role
		conf *workspacev1.ProjectConfig
	)
	err := s.tx(ctx, func(q *store.Queries) error {
		_, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: name})
		if err == nil {
			return connect.NewError(connect.CodeAlreadyExists, errorf("project %q exists", name))
		}
		if !noRows(err) {
			return internal(err)
		}
		if p, err = q.CreateProject(ctx, store.CreateProjectParams{ID: newID("prj"), WorkspaceID: c.workspace, Name: name, CreatedBy: c.userID, CreatedAt: s.now()}); err != nil {
			return internal(err)
		}
		// A teamlead who creates a project leads it; the admin has every project anyway.
		if c.subject.Role != authz.Admin {
			role = authz.Teamlead
			if err := q.SetProjectRole(ctx, store.SetProjectRoleParams{WorkspaceID: c.workspace, ProjectID: p.ID, UserID: c.userID, Role: string(role)}); err != nil {
				return internal(err)
			}
		}
		if err := s.audit(ctx, q, c.workspace, "project.created", name, ""); err != nil {
			return internal(err)
		}
		if conf, err = s.addVersion(ctx, q, p, cfg, message); err != nil {
			return err
		}
		p.Version = conf.Version
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.CreateProjectResponse{Project: projectInfo(p, role), Config: conf}), nil
}

func (s *Service) DeleteProject(ctx context.Context, req *connect.Request[workspacev1.DeleteProjectRequest]) (*connect.Response[workspacev1.DeleteProjectResponse], error) {
	c := callerOf(ctx)
	if c.subject.Role != authz.Admin && c.subject.Projects[req.Msg.Project] != authz.Teamlead {
		return nil, denied("deleting project %q is for its teamleads and the workspace admin", req.Msg.Project)
	}
	if req.Msg.ConfirmName != req.Msg.Project {
		return nil, invalid("to delete the project, repeat its name %q", req.Msg.Project)
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		p, err := q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: c.workspace, Name: req.Msg.Project})
		if noRows(err) {
			return notFound("no project %q in this workspace", req.Msg.Project)
		}
		if err != nil {
			return internal(err)
		}
		if err := q.DeleteProject(ctx, p.ID); err != nil {
			return internal(err)
		}
		return s.audit(ctx, q, c.workspace, "project.deleted", p.Name, "")
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.DeleteProjectResponse{}), nil
}

func (s *Service) project(ctx context.Context, name string) (store.Project, error) {
	p, err := s.q.ProjectByName(ctx, store.ProjectByNameParams{WorkspaceID: callerOf(ctx).workspace, Name: name})
	if noRows(err) {
		return p, notFound("no project %q in this workspace", name)
	}
	if err != nil {
		return p, internal(err)
	}
	return p, nil
}

func (s *Service) GetProjectConfig(ctx context.Context, req *connect.Request[workspacev1.GetProjectConfigRequest]) (*connect.Response[workspacev1.GetProjectConfigResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	v := req.Msg.Version
	if v == 0 {
		v = p.Version
	}
	row, err := s.q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: v})
	if noRows(err) {
		return nil, notFound("%s has no version %d (the current one is %d)", p.Name, v, p.Version)
	}
	if err != nil {
		return nil, internal(err)
	}
	return connect.NewResponse(&workspacev1.GetProjectConfigResponse{Config: configInfo(row), CurrentVersion: p.Version}), nil
}

func (s *Service) ListProjectVersions(ctx context.Context, req *connect.Request[workspacev1.ListProjectVersionsRequest]) (*connect.Response[workspacev1.ListProjectVersionsResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	limit := req.Msg.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := s.q.ProjectVersions(ctx, store.ProjectVersionsParams{ProjectID: p.ID, Limit: limit})
	if err != nil {
		return nil, internal(err)
	}
	resp := &workspacev1.ListProjectVersionsResponse{}
	for _, r := range rows {
		resp.Versions = append(resp.Versions, &workspacev1.ProjectVersion{Version: r.Version, Author: r.Author, Message: r.Message, CreatedAt: timestamppb.New(r.CreatedAt), Signed: r.Signed})
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) UpdateProjectSettings(ctx context.Context, req *connect.Request[workspacev1.UpdateProjectSettingsRequest]) (*connect.Response[workspacev1.UpdateProjectSettingsResponse], error) {
	m := req.Msg
	if m.Domain != "" {
		if err := checkDomain(m.Domain); err != nil {
			return nil, err
		}
	}
	var policy struct {
		Features  *spec.FeaturePolicy  `yaml:"features"`
		Overrides *spec.OverridePolicy `yaml:"overrides"`
		Data      *spec.DataPolicy     `yaml:"data"`
	}
	if strings.TrimSpace(m.PolicyYaml) != "" {
		if err := strictYAML(m.PolicyYaml, &policy); err != nil {
			return nil, invalid("policy: %v", err)
		}
	}
	conf, err := s.change(ctx, m.Project, m.BaseVersion, func(cfg *spec.Project) (string, error) {
		var what []string
		if m.Domain != "" && m.Domain != cfg.Domain {
			cfg.Domain = m.Domain
			what = append(what, "domain "+m.Domain)
		}
		if policy.Features != nil {
			cfg.Features, what = policy.Features, append(what, "features")
		}
		if policy.Overrides != nil {
			cfg.Overrides, what = policy.Overrides, append(what, "overrides")
		}
		if policy.Data != nil {
			cfg.Data, what = policy.Data, append(what, "data")
		}
		return "project set " + strings.Join(what, ", "), nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.UpdateProjectSettingsResponse{Config: conf}), nil
}

func (s *Service) RollbackProject(ctx context.Context, req *connect.Request[workspacev1.RollbackProjectRequest]) (*connect.Response[workspacev1.RollbackProjectResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	old, err := s.q.ProjectVersion(ctx, store.ProjectVersionParams{ProjectID: p.ID, Version: req.Msg.Version})
	if noRows(err) {
		return nil, notFound("%s has no version %d", p.Name, req.Msg.Version)
	}
	if err != nil {
		return nil, internal(err)
	}
	target, err := spec.ParseYAML([]byte(old.Config))
	if err != nil {
		return nil, internal(err)
	}
	conf, err := s.change(ctx, p.Name, 0, func(cfg *spec.Project) (string, error) {
		*cfg = *target
		return fmt.Sprintf("rollback to %d", old.Version), nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RollbackProjectResponse{Config: conf}), nil
}

func (s *Service) PutApp(ctx context.Context, req *connect.Request[workspacev1.PutAppRequest]) (*connect.Response[workspacev1.PutAppResponse], error) {
	m := req.Msg
	var app spec.App
	if err := strictYAML(m.AppYaml, &app); err != nil {
		return nil, invalid("application: %v", err)
	}
	conf, err := s.change(ctx, m.Project, m.BaseVersion, func(cfg *spec.Project) (string, error) {
		_, exists := cfg.Apps[m.Name]
		switch {
		case m.Create && exists:
			return "", connect.NewError(connect.CodeAlreadyExists, errorf("%s already has an application %q", m.Project, m.Name))
		case !m.Create && !exists:
			return "", notFound("%s has no application %q", m.Project, m.Name)
		}
		cfg.Apps[m.Name] = &app
		if m.Create {
			return "app add " + m.Name, nil
		}
		return "app set " + m.Name, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.PutAppResponse{Config: conf}), nil
}

func (s *Service) RemoveApp(ctx context.Context, req *connect.Request[workspacev1.RemoveAppRequest]) (*connect.Response[workspacev1.RemoveAppResponse], error) {
	m := req.Msg
	c := callerOf(ctx)
	conf, err := s.change(ctx, m.Project, m.BaseVersion, func(cfg *spec.Project) (string, error) {
		if cfg.Apps[m.Name] == nil {
			return "", notFound("%s has no application %q", m.Project, m.Name)
		}
		if attached := cfg.Attached(m.Name); len(attached) > 0 {
			if !c.subject.Can(authz.ServicesManage, m.Project) {
				return "", denied("services %s are attached to %s: detaching them needs %s", strings.Join(attached, ", "), m.Name, authz.ServicesManage)
			}
			for _, n := range attached {
				svc := cfg.Services[n]
				svc.Apps = slices.DeleteFunc(svc.Apps, func(a string) bool { return a == m.Name })
			}
		}
		delete(cfg.Apps, m.Name)
		return "app rm " + m.Name, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveAppResponse{Config: conf}), nil
}

func (s *Service) PutService(ctx context.Context, req *connect.Request[workspacev1.PutServiceRequest]) (*connect.Response[workspacev1.PutServiceResponse], error) {
	m := req.Msg
	var svc spec.Service
	if err := strictYAML(m.ServiceYaml, &svc); err != nil {
		return nil, invalid("service: %v", err)
	}
	// The catalog's name for the type: node → nodejs.
	if t, ok := catalog.Lookup(svc.Type); ok {
		svc.Type = t.Name
	}
	conf, err := s.change(ctx, m.Project, m.BaseVersion, func(cfg *spec.Project) (string, error) {
		_, exists := cfg.Services[m.Name]
		switch {
		case m.Create && exists:
			return "", connect.NewError(connect.CodeAlreadyExists, errorf("%s already has a service %q", m.Project, m.Name))
		case !m.Create && !exists:
			return "", notFound("%s has no service %q", m.Project, m.Name)
		}
		cfg.Services[m.Name] = &svc
		what := strings.TrimSpace(svc.Type + " " + svc.Version)
		if m.Create {
			return fmt.Sprintf("service add %s (%s)", m.Name, what), nil
		}
		return fmt.Sprintf("service set %s (%s)", m.Name, what), nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.PutServiceResponse{Config: conf}), nil
}

func (s *Service) RemoveService(ctx context.Context, req *connect.Request[workspacev1.RemoveServiceRequest]) (*connect.Response[workspacev1.RemoveServiceResponse], error) {
	m := req.Msg
	conf, err := s.change(ctx, m.Project, m.BaseVersion, func(cfg *spec.Project) (string, error) {
		if cfg.Services[m.Name] == nil {
			return "", notFound("%s has no service %q", m.Project, m.Name)
		}
		delete(cfg.Services, m.Name)
		return "service rm " + m.Name, nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.RemoveServiceResponse{Config: conf}), nil
}

// strictYAML decodes YAML; unknown keys are errors.
func strictYAML(src string, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(src)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

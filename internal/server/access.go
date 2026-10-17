package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/auth/token"
	"github.com/amalychev/devopus/internal/authz"
	"github.com/amalychev/devopus/internal/server/store"
)

// caller is the signed-in user of a call and, in a workspace, what they may do there.
type caller struct {
	userID, email string
	// expires is when the access token stops working: long streams end there.
	expires   time.Time
	workspace string
	subject   authz.Subject
	// projectIDs maps the names of the projects the caller has a role in to their ids.
	projectIDs map[string]string
	// server is set for an admin's server signed in with its device token: its
	// workspace, which it may not leave.
	server string
}

// serverMethods are all an admin's server may call: its settings, its statuses and
// its device key. A stolen server token reads nothing else.
var serverMethods = map[string]bool{
	"/devopus.sync.v1.SyncService/Watch":                    true,
	"/devopus.sync.v1.SyncService/ReportStatus":             true,
	"/devopus.workspace.v1.WorkspaceService/RegisterDeviceKey": true,
}

type callerKey struct{}

func callerOf(ctx context.Context) *caller {
	c, _ := ctx.Value(callerKey{}).(*caller)
	if c == nil {
		return &caller{}
	}
	return c
}

// AccessOf is the access a method declares in proto; nil when it declares none.
func AccessOf(m protoreflect.MethodDescriptor) *workspacev1.Access {
	a, _ := proto.GetExtension(m.Options(), workspacev1.E_Access).(*workspacev1.Access)
	if a == nil || a.Scope == workspacev1.Scope_SCOPE_UNSPECIFIED {
		return nil
	}
	return a
}

// accessInterceptor is the one place permissions are checked: it reads the access a
// method declares, authenticates the caller and checks the permission in the
// request's workspace or project. A method that declares nothing is refused. Streams
// are checked on their request message, before the handler sees it.
func (s *Service) accessInterceptor() connect.Interceptor { return &access{s: s} }

type access struct{ s *Service }

func (i *access) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		msg, _ := req.Any().(proto.Message)
		c, err := i.s.authorize(ctx, req.Spec(), req.Header().Get("Authorization"), msg)
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, callerKey{}, c), req)
	}
}

func (i *access) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *access) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		c := &caller{} // filled once the request arrives
		return next(context.WithValue(ctx, callerKey{}, c), &checkedConn{StreamingHandlerConn: conn, s: i.s, ctx: ctx, c: c})
	}
}

// checkedConn checks the access of a stream on its first message.
type checkedConn struct {
	connect.StreamingHandlerConn
	s       *Service
	ctx     context.Context
	c       *caller
	checked bool
}

func (cc *checkedConn) Receive(m any) error {
	if err := cc.StreamingHandlerConn.Receive(m); err != nil {
		return err
	}
	if cc.checked {
		return nil
	}
	msg, _ := m.(proto.Message)
	c, err := cc.s.authorize(cc.ctx, cc.Spec(), cc.RequestHeader().Get("Authorization"), msg)
	if err != nil {
		return err
	}
	*cc.c, cc.checked = *c, true
	return nil
}

func (s *Service) authorize(ctx context.Context, spec connect.Spec, header string, msg proto.Message) (*caller, error) {
	m, _ := spec.Schema.(protoreflect.MethodDescriptor)
	var access *workspacev1.Access
	if m != nil {
		access = AccessOf(m)
	}
	if access == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New(spec.Procedure+" declares no access"))
	}
	if access.Scope == workspacev1.Scope_SCOPE_NONE {
		return &caller{}, nil // the request carries its own credential
	}
	c, err := s.authenticate(ctx, header)
	if err != nil {
		return nil, err
	}
	if c.server != "" && !serverMethods[spec.Procedure] {
		return nil, denied("a server's token is for its settings and statuses only")
	}
	if access.Scope == workspacev1.Scope_SCOPE_SIGNED_IN {
		return c, nil
	}
	if msg == nil {
		return nil, internal(errors.New("request is not a message"))
	}
	ws := field(msg, "workspace_id")
	if ws == "" {
		return nil, invalid("the workspace is not set: 'devopus workspace use <name>'")
	}
	if c.server != "" && ws != c.server {
		return nil, denied("a server's token is for its own workspace")
	}
	project := ""
	if access.Scope == workspacev1.Scope_SCOPE_PROJECT {
		if project = field(msg, "project"); project == "" {
			return nil, invalid("the project is not set: --project <name>")
		}
	}
	if err := s.load(ctx, c, ws); err != nil {
		return nil, err
	}
	perm := authz.Permission(access.Permission)
	if !c.subject.Can(perm, project) {
		if project != "" {
			return nil, denied("this needs the permission %s in project %q; ask its teamlead or the workspace admin", perm, project)
		}
		return nil, denied("this needs the permission %s in the workspace; ask its admin", perm)
	}
	return c, nil
}

// field reads a string field of a request by name.
func field(m proto.Message, name protoreflect.Name) string {
	r := m.ProtoReflect()
	f := r.Descriptor().Fields().ByName(name)
	if f == nil || f.Kind() != protoreflect.StringKind {
		return ""
	}
	return strings.TrimSpace(r.Get(f).String())
}

// authenticate checks the access token: Auth signed it for Devopus and it has not
// expired. Revoking a member works on the next call: membership is read every time.
func (s *Service) authenticate(ctx context.Context, header string) (*caller, error) {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first: devopus login"))
	}
	if strings.HasPrefix(raw, serverTokenPrefix) {
		return s.authenticateServer(ctx, raw)
	}
	claims, err := token.Verify(raw, s.cfg.Keys, token.Expect{Issuer: s.cfg.AuthIssuer, Audience: s.cfg.PublicURL, Now: s.cfg.Now()})
	if errors.Is(err, token.ErrClaims) {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("the token is not for this server (%s): sign in again, or update devopus", s.cfg.PublicURL))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if claims.Email == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the token has no email; sign in again: devopus login"))
	}
	c := &caller{userID: claims.Subject, email: normalizeEmail(claims.Email), expires: claims.ExpiresAt()}
	if e, ok := s.seen.Load(c.userID); !ok || e != c.email {
		if err := s.q.UpsertUser(ctx, store.UpsertUserParams{ID: c.userID, Email: c.email, SeenAt: s.now()}); err != nil {
			return nil, internal(err)
		}
		s.seen.Store(c.userID, c.email)
	}
	return c, nil
}

// load reads what the caller may do in a workspace. A workspace they are not in, or
// that does not exist, is refused the same way.
func (s *Service) load(ctx context.Context, c *caller, ws string) error {
	m, err := s.q.Member(ctx, store.MemberParams{WorkspaceID: ws, UserID: c.userID})
	if noRows(err) {
		return denied("you are not a member of workspace %s", ws)
	}
	if err != nil {
		return internal(err)
	}
	sub, ids, err := s.subject(ctx, s.q, ws, c.userID, authz.Role(m.Role))
	if err != nil {
		return internal(err)
	}
	c.workspace, c.subject, c.projectIDs = ws, sub, ids
	return nil
}

// subject collects a member's roles and grants in projects.
func (s *Service) subject(ctx context.Context, q *store.Queries, ws, user string, role authz.Role) (authz.Subject, map[string]string, error) {
	sub := authz.Subject{Role: role, Projects: map[string]authz.Role{}, Grants: map[string][]authz.Permission{}}
	ids := map[string]string{}
	roles, err := q.UserProjectRoles(ctx, store.UserProjectRolesParams{WorkspaceID: ws, UserID: user})
	if err != nil {
		return sub, nil, err
	}
	for _, r := range roles {
		sub.Projects[r.Project] = authz.Role(r.Role)
		ids[r.Project] = r.ProjectID
	}
	grants, err := q.UserGrants(ctx, store.UserGrantsParams{WorkspaceID: ws, UserID: user})
	if err != nil {
		return sub, nil, err
	}
	for _, g := range grants {
		sub.Grants[g.Project] = append(sub.Grants[g.Project], authz.Permission(g.Permission))
	}
	return sub, ids, nil
}

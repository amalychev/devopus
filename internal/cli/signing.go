package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/signing"
	"github.com/amalychev/devopus/internal/spec"
)

// signVersion signs the version the caller just made (ADR-0018), when the server's
// version is what they asked for: want checks it against their change. The team's
// agents apply a signed version that runs new code without asking, so nothing is
// signed that the person did not send. The change is made either way: a failure
// here is told, not returned.
func (a *app) signVersion(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient, project string,
	conf *workspacev1.ProjectConfig, want func(got *spec.Project) error) {
	warn := func(format string, args ...any) {
		fmt.Fprintf(a.stderr, "Version %d of %s is not signed: %s\n", conf.Version, project, fmt.Sprintf(format, args...))
	}
	got, err := spec.ParseYAML([]byte(conf.Yaml))
	if err != nil {
		warn("%v", err)
		return
	}
	if want != nil {
		if err := want(got); err != nil {
			warn("the server stored another version than your change — %v. Look at it: 'devopus project config show %s'", err, project)
			return
		}
	}
	key, certified, err := a.signingKey(ctx, s, cur, c)
	if err != nil {
		warn("%v", err)
		return
	}
	sig := key.Sign(signing.SettingsMessage(cur.ID, project, conf.Version, []byte(conf.Yaml)))
	if _, err := c.SignVersion(ctx, req(s, &workspacev1.SignVersionRequest{WorkspaceId: cur.ID, Project: project, Version: conf.Version,
		DeviceId: credentials.DeviceID(), Signature: sig})); err != nil {
		warn("%v", rpcErr(err))
		return
	}
	if !certified {
		fmt.Fprintf(a.stderr, "Signed, but the workspace's admin has not certified this device yet: the team's agents ask before applying new code from it.\n"+
			"The admin certifies it with 'devopus trust approve'.\n")
	}
}

// signingKey is this device's signing key, registered in the workspace, and whether
// it counts: certified, or the root. The workspace's admin makes the first root of
// trust from the device they sign on first.
func (a *app) signingKey(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient) (signing.Key, bool, error) {
	key, err := credentials.SigningKey()
	if err != nil {
		return key, false, err
	}
	reg, err := c.RegisterSigningKey(ctx, req(s, &workspacev1.RegisterSigningKeyRequest{WorkspaceId: cur.ID, DeviceId: credentials.DeviceID(),
		DeviceName: credentials.DeviceName(), PublicKey: key.Public()}))
	if err != nil {
		return key, false, rpcErr(err)
	}
	if reg.Msg.Certified {
		return key, true, nil
	}
	keys, err := c.ListSigningKeys(ctx, req(s, &workspacev1.ListSigningKeysRequest{WorkspaceId: cur.ID}))
	if err != nil || keys.Msg.Root != nil {
		return key, false, nil
	}
	_, err = c.SetTrustRoot(ctx, req(s, &workspacev1.SetTrustRootRequest{WorkspaceId: cur.ID, DeviceId: credentials.DeviceID()}))
	if connect.CodeOf(err) == connect.CodePermissionDenied {
		return key, false, nil // not the admin: the admin's device makes the root
	}
	if err != nil {
		return key, false, rpcErr(err)
	}
	fmt.Fprintf(a.stderr, "This device's signing key is now the root of trust of %s: %s.\n"+
		"Your team's agents check every signature against it. Tell them this fingerprint; 'devopus trust show' shows theirs.\n",
		cur.Name, signing.Fingerprint(key.Public()))
	return key, true, nil
}

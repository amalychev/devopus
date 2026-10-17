package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

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

// projectSignCmd signs a version someone reviewed: a change made in the web, which
// cannot sign — the page comes from the workspace server, whose word signatures exist
// not to take.
func (a *app) projectSignCmd() *cobra.Command {
	var version int32
	var yes bool
	cmd := &cobra.Command{
		Use:   "sign <project> [--version N]",
		Short: "Review an unsigned version (a change made in the web) and sign it",
		Long: "Servers of the admin run signed versions only, and developers' agents ask before\n" +
			"unsigned new code. Changes made with devopus are signed as they are made; one made in\n" +
			"the web is not. This shows what the version changes against the last signed one —\n" +
			"new code marked with ! — asks, and signs it with this device. It takes the author or\n" +
			"someone with project.configure in the project.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			project := args[0]
			res, err := c.GetProjectConfig(ctx, req(s, &workspacev1.GetProjectConfigRequest{WorkspaceId: cur.ID, Project: project, Version: version}))
			if err != nil {
				return rpcErr(err)
			}
			conf := res.Msg.Config
			if len(conf.Signature) > 0 {
				return a.done(map[string]any{"project": project, "version": conf.Version, "signed": true}, "Version %d of %s is signed already.\n", conf.Version, project)
			}
			question := fmt.Sprintf("Sign version %d of %s (%s, by %s)?", conf.Version, project, conf.Message, conf.Author)
			question += a.sinceSigned(ctx, s, cur, c, project, conf)
			ok, err := a.confirm(yes, question)
			if err != nil {
				return err
			}
			if !ok {
				return fail(ExitError, "nothing signed")
			}
			key, certified, err := a.signingKey(ctx, s, cur, c)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			sig := key.Sign(signing.SettingsMessage(cur.ID, project, conf.Version, []byte(conf.Yaml)))
			if _, err := c.SignVersion(ctx, req(s, &workspacev1.SignVersionRequest{WorkspaceId: cur.ID, Project: project, Version: conf.Version,
				DeviceId: credentials.DeviceID(), Signature: sig})); err != nil {
				return rpcErr(err)
			}
			if !certified {
				fmt.Fprintln(a.stderr, "Signed, but the workspace's admin has not certified this device yet: the signature counts once they do ('devopus trust approve').")
			}
			return a.done(map[string]any{"project": project, "version": conf.Version, "signed": true}, "✓ Version %d of %s is signed.\n", conf.Version, project)
		},
	}
	cmd.Flags().Int32Var(&version, "version", 0, "the version (default: the newest)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}

// sinceSigned is what a version changes against the newest signed one before it.
func (a *app) sinceSigned(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient, project string, conf *workspacev1.ProjectConfig) string {
	hist, err := c.ListProjectVersions(ctx, req(s, &workspacev1.ListProjectVersionsRequest{WorkspaceId: cur.ID, Project: project, Limit: 100}))
	if err != nil {
		return ""
	}
	var base int32
	for _, v := range hist.Msg.Versions {
		if v.Version < conf.Version && v.Signed {
			base = v.Version
			break
		}
	}
	if base == 0 {
		return "\n  No version before it is signed: review the whole settings ('devopus project config show " + project + "')."
	}
	old, err := c.GetProjectConfig(ctx, req(s, &workspacev1.GetProjectConfigRequest{WorkspaceId: cur.ID, Project: project, Version: base}))
	if err != nil {
		return ""
	}
	a1, err1 := spec.ParseYAML([]byte(old.Msg.Config.Yaml))
	a2, err2 := spec.ParseYAML([]byte(conf.Yaml))
	if err1 != nil || err2 != nil {
		return ""
	}
	changes := spec.Diff(a1, a2)
	if len(changes) == 0 {
		return fmt.Sprintf("\n  Against v%d, the last signed one, nothing changes.", base)
	}
	out := fmt.Sprintf("\n  Against v%d, the last signed one:", base)
	for _, ch := range changes {
		mark := " "
		if ch.Risky {
			mark = "!"
		}
		out += fmt.Sprintf("\n  %s %s", mark, ch.What)
	}
	return out + "\n  (! — new code on the machines and servers that run it)"
}

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"filippo.io/age"
	"github.com/spf13/cobra"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/gen/devopus/workspace/v1/workspacev1connect"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/secrets"
)

// Secrets are end-to-end (ADR-0009): values are encrypted here, to the workspace's
// public key, and only agents decrypt them. The workspace's private key lives on
// devices, wrapped for each; the admin also holds it as a recovery key.

// keyOf is the workspace's key as this device sees it.
type keyOf struct {
	generation int32
	public     string
	wrapped    []byte
}

// workspaceKey registers this device's key and returns the workspace's. With create
// and no key yet, the admin's device makes one and shows its recovery key once.
func (a *app) workspaceKey(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient, create bool) (keyOf, error) {
	dk, err := credentials.DeviceKey()
	if err != nil {
		return keyOf{}, fail(ExitError, "the device key: %v", err)
	}
	device := credentials.DeviceID()
	if _, err := c.RegisterDeviceKey(ctx, req(s, &workspacev1.RegisterDeviceKeyRequest{WorkspaceId: cur.ID, DeviceId: device,
		DeviceName: credentials.DeviceName(), PublicKey: dk.Recipient().String()})); err != nil {
		return keyOf{}, rpcErr(err)
	}
	res, err := c.GetSecretsKey(ctx, req(s, &workspacev1.GetSecretsKeyRequest{WorkspaceId: cur.ID, DeviceId: device}))
	if err != nil {
		return keyOf{}, rpcErr(err)
	}
	k := keyOf{generation: res.Msg.Generation, public: res.Msg.PublicKey, wrapped: res.Msg.Wrapped}
	if k.generation > 0 || !create {
		return k, nil
	}
	key, err := secrets.NewKey()
	if err != nil {
		return keyOf{}, err
	}
	wrapped, err := secrets.Wrap(key, dk.Recipient().String())
	if err != nil {
		return keyOf{}, err
	}
	made, err := c.CreateSecretsKey(ctx, req(s, &workspacev1.CreateSecretsKeyRequest{WorkspaceId: cur.ID, PublicKey: key.Recipient().String(), DeviceId: device, Wrapped: wrapped}))
	if connect.CodeOf(err) == connect.CodePermissionDenied {
		return keyOf{}, fail(ExitAuth, "%s has no secrets key yet: its admin makes it with the first secret", cur.Name)
	}
	if err != nil {
		return keyOf{}, rpcErr(err)
	}
	a.showRecoveryKey(cur.Name, key)
	return keyOf{generation: made.Msg.Generation, public: key.Recipient().String(), wrapped: wrapped}, nil
}

func (a *app) showRecoveryKey(workspace string, key *age.X25519Identity) {
	fmt.Fprintf(a.stderr, `
The secrets of %s are encrypted with a key only your team's devices hold; Devopus
cannot read them. Keep this recovery key in your password manager: it brings the key
back if no device has it any more ('devopus secrets recover'). It is shown only now.

  %s

`, workspace, key.String())
}

// seal encrypts a value for the workspace and stores it, once more if the key was
// rotated meanwhile.
func (a *app) seal(ctx context.Context, s *session, cur *current, c workspacev1connect.WorkspaceServiceClient, project, name string, value []byte) error {
	for try := 0; ; try++ {
		k, err := a.workspaceKey(ctx, s, cur, c, true)
		if err != nil {
			return err
		}
		sealed, err := secrets.Seal(value, k.public)
		if err != nil {
			return err
		}
		_, err = c.SetSecret(ctx, req(s, &workspacev1.SetSecretRequest{WorkspaceId: cur.ID, Project: project, Name: name, Generation: k.generation, Ciphertext: sealed}))
		if connect.CodeOf(err) == connect.CodeAborted && try < 2 {
			continue
		}
		if err != nil {
			return rpcErr(err)
		}
		return nil
	}
}

func (a *app) secretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Secrets of projects, end-to-end encrypted: set, list, rm, rotate, recover",
		Long: "Secrets are values like passwords that settings refer to as ${{ secrets.NAME }}. They\n" +
			"are encrypted on this machine to the workspace's key; the workspace server keeps only\n" +
			"ciphertexts, and agents decrypt them where the environments run. A new member's\n" +
			"agent gets the key from an admin's or a teamlead's agent that is online.",
	}
	set := &cobra.Command{
		Use:   "set <project> <NAME> [value]",
		Short: "Set a secret (admin, teamlead, secrets.manage); without a value it is read from stdin",
		Example: "  devopus secrets set shop DB_PASSWORD\n" +
			"  printf %s \"$TOKEN\" | devopus secrets set shop STRIPE_KEY",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var value []byte
			if len(args) == 3 {
				value = []byte(args[2])
			} else {
				if f, ok := a.stdin.(*os.File); ok && isTerminal(f) {
					fmt.Fprintf(a.stderr, "Value of %s: ", args[1])
					line, _ := bufio.NewReader(a.stdin).ReadString('\n')
					value = []byte(strings.TrimRight(line, "\r\n"))
				} else {
					b, err := io.ReadAll(a.stdin)
					if err != nil {
						return err
					}
					value = []byte(strings.TrimRight(string(b), "\r\n"))
				}
			}
			if len(value) == 0 {
				return fail(ExitInvalid, "the value is empty")
			}
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if err := a.seal(cmd.Context(), s, cur, c, args[0], args[1], value); err != nil {
				return err
			}
			return a.done(map[string]any{"project": args[0], "name": args[1]}, "Secret %s of %s is set; agents pick it up on their own.\n", args[1], args[0])
		},
	}
	list := &cobra.Command{
		Use:   "list <project>",
		Short: "Names of a project's secrets; values only agents decrypt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListSecrets(cmd.Context(), req(s, &workspacev1.ListSecretsRequest{WorkspaceId: cur.ID, Project: args[0]}))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeProto(res.Msg)
			}
			if len(res.Msg.Secrets) == 0 {
				fmt.Fprintf(a.stdout, "%s has no secrets: devopus secrets set %s <NAME>\n", args[0], args[0])
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SECRET\tSET BY\tWHEN")
			for _, sec := range res.Msg.Secrets {
				fmt.Fprintf(w, "%s\t%s\t%s\n", sec.Name, sec.UpdatedBy, sec.UpdatedAt.AsTime().Local().Format("2 Jan 2006 15:04"))
			}
			return w.Flush()
		},
	}
	rm := &cobra.Command{
		Use:   "rm <project> <NAME>",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, cur, c, err := a.inWorkspace(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := c.RemoveSecret(cmd.Context(), req(s, &workspacev1.RemoveSecretRequest{WorkspaceId: cur.ID, Project: args[0], Name: args[1]})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"project": args[0], "removed": args[1]}, "Secret %s of %s removed.\n", args[1], args[0])
		},
	}
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Replace the workspace's key: re-encrypt every secret on this machine (admin)",
		Long: "Makes a new workspace key, re-encrypts every secret with it here and wraps it for the\n" +
			"devices of the workspace's members: after removing a member or losing a device. Needs\n" +
			"the current key on this machine; prints the new recovery key.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			k, err := a.workspaceKey(ctx, s, cur, c, false)
			if err != nil {
				return err
			}
			if k.generation == 0 {
				return fail(ExitInvalid, "%s has no secrets key yet: nothing to rotate", cur.Name)
			}
			old, err := a.heldKey(k)
			if err != nil {
				return err
			}
			prep, err := c.PrepareRotation(ctx, req(s, &workspacev1.PrepareRotationRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			next, err := secrets.NewKey()
			if err != nil {
				return err
			}
			rot := &workspacev1.RotateSecretsKeyRequest{WorkspaceId: cur.ID, FromGeneration: prep.Msg.Generation, PublicKey: next.Recipient().String()}
			for _, sec := range prep.Msg.Secrets {
				v, err := secrets.Open(sec.Ciphertext, old)
				if err != nil {
					return fail(ExitError, "secret %s of %s does not open with the key here: %v", sec.Name, sec.Project, err)
				}
				sealed, err := secrets.Seal(v, next.Recipient().String())
				if err != nil {
					return err
				}
				rot.Secrets = append(rot.Secrets, &workspacev1.SealedSecret{Project: sec.Project, Name: sec.Name, Ciphertext: sealed})
			}
			for _, d := range prep.Msg.Devices {
				wrapped, err := secrets.Wrap(next, d.PublicKey)
				if err != nil {
					continue
				}
				rot.Wraps = append(rot.Wraps, &workspacev1.KeyWrap{DeviceId: d.DeviceId, UserId: d.UserId, Wrapped: wrapped})
			}
			res, err := c.RotateSecretsKey(ctx, req(s, rot))
			if err != nil {
				return rpcErr(err)
			}
			a.showRecoveryKey(cur.Name, next)
			return a.done(map[string]any{"generation": res.Msg.Generation, "secrets": len(rot.Secrets), "devices": len(rot.Wraps)},
				"The key of %s is replaced (generation %d): %d secrets re-encrypted, %d devices keep access.\n", cur.Name, res.Msg.Generation, len(rot.Secrets), len(rot.Wraps))
		},
	}
	recover := &cobra.Command{
		Use:   "recover",
		Short: "Put the workspace's key on this machine from the recovery key (admin)",
		Long: "Reads the recovery key from stdin and puts the workspace's key on this machine: when\n" +
			"no device holds it any more. Its agent then delivers it to the devices waiting for it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			fmt.Fprint(a.stderr, "Recovery key: ")
			line, _ := bufio.NewReader(a.stdin).ReadString('\n')
			key, err := secrets.ParseKey(line)
			if err != nil {
				return fail(ExitInvalid, "%v", err)
			}
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			k, err := a.workspaceKey(ctx, s, cur, c, false)
			if err != nil {
				return err
			}
			if k.generation == 0 || k.public != key.Recipient().String() {
				return fail(ExitInvalid, "this is not the current key of %s (it was rotated since?)", cur.Name)
			}
			dk, err := credentials.DeviceKey()
			if err != nil {
				return err
			}
			wrapped, err := secrets.Wrap(key, dk.Recipient().String())
			if err != nil {
				return err
			}
			if _, err := c.ProvideKeyWraps(ctx, req(s, &workspacev1.ProvideKeyWrapsRequest{WorkspaceId: cur.ID, Generation: k.generation,
				Wraps: []*workspacev1.KeyWrap{{DeviceId: credentials.DeviceID(), UserId: s.acc.Session.UserID, Wrapped: wrapped}}})); err != nil {
				return rpcErr(err)
			}
			return a.done(map[string]any{"generation": k.generation}, "The key of %s is on this machine again.\n", cur.Name)
		},
	}
	cmd.AddCommand(set, list, rm, rotate, recover)
	return cmd
}

// heldKey opens the workspace's key wrapped for this device.
func (a *app) heldKey(k keyOf) (*age.X25519Identity, error) {
	if len(k.wrapped) == 0 {
		return nil, fail(ExitInvalid, "the workspace's key is not on this machine: 'devopus secrets recover' with the recovery key")
	}
	dk, err := credentials.DeviceKey()
	if err != nil {
		return nil, err
	}
	key, err := secrets.Unwrap(k.wrapped, dk)
	if err != nil {
		return nil, fail(ExitError, "%v", err)
	}
	return key, nil
}

// uploadSecrets encrypts the values an import found and stores them in the project.
func (a *app) uploadSecrets(ctx context.Context, project string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	s, cur, c, err := a.inWorkspace(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := a.seal(ctx, s, cur, c, project, n, []byte(values[n])); err != nil {
			return err
		}
	}
	if !a.json {
		fmt.Fprintf(a.stderr, "Secrets %s are encrypted here and stored in %s; the server cannot read them.\n", strings.Join(names, ", "), project)
	}
	return nil
}

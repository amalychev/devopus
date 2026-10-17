package cli

import (
	"bytes"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/credentials"
	"github.com/amalychev/devopus/internal/signing"
)

type signingKeyRow struct {
	Email       string `json:"email"`
	Role        string `json:"role"`
	Device      string `json:"device"`
	DeviceID    string `json:"device_id"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"` // root, certified, not certified
	ThisDevice  bool   `json:"this_device"`
}

func (a *app) trustCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Signatures of settings: the workspace's root of trust and the devices it certified",
		Long: "Every version of a project's settings is signed by its author's device. The team's\n" +
			"agents apply a version that runs new code — an image, a command, a hook — without\n" +
			"asking only when a device the workspace's admin certified signed it; they check\n" +
			"that against the root of trust, a key of the admin's device, which each agent\n" +
			"remembers the first time (ADR-0018).",
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "The root of trust and every device's signing key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			res, err := c.ListSigningKeys(ctx, req(s, &workspacev1.ListSigningKeysRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			rows := keyRows(res.Msg)
			if a.json {
				out := map[string]any{"keys": rows}
				if r := res.Msg.Root; r != nil {
					out["root"] = map[string]any{"fingerprint": signing.Fingerprint(r.PublicKey), "email": r.UserEmail, "device": r.DeviceName}
				}
				return a.writeJSON(out)
			}
			if r := res.Msg.Root; r != nil {
				fmt.Fprintf(a.stdout, "Root of trust  %s (%s, device %s)\n", signing.Fingerprint(r.PublicKey), r.UserEmail, r.DeviceName)
			} else {
				fmt.Fprintln(a.stdout, "Root of trust  none yet: the admin's first signed change makes it")
			}
			if len(rows) == 0 {
				return nil
			}
			fmt.Fprintln(a.stdout)
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WHO\tROLE\tDEVICE\tFINGERPRINT\tSTATUS")
			for _, r := range rows {
				device := r.Device
				if r.ThisDevice {
					device += " (this one)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Email, r.Role, device, r.Fingerprint, r.Status)
			}
			return w.Flush()
		},
	}
	var yes bool
	approve := &cobra.Command{
		Use:   "approve [email|device…]",
		Short: "Certify devices' signing keys with the root (admin, on the root's device)",
		Long: "Certifies the devices of people you trust to change settings: their signed versions\n" +
			"then run on the team's machines without asking. Without arguments, every device\n" +
			"not certified yet, one by one. Compare each fingerprint with the one the person\n" +
			"sees in 'devopus trust show' on their machine before you answer.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			key, _, err := a.signingKey(ctx, s, cur, c)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			res, err := c.ListSigningKeys(ctx, req(s, &workspacev1.ListSigningKeysRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			root := res.Msg.Root
			if root == nil || !bytes.Equal(root.PublicKey, key.Public()) {
				return fail(ExitInvalid, "certificates are made on the device that holds the root of trust (%s): this is not it", rootWhere(root))
			}
			var done []string
			for _, k := range res.Msg.Keys {
				if len(k.Certificate) > 0 || bytes.Equal(k.PublicKey, root.PublicKey) || !wanted(k, args) {
					continue
				}
				ok, err := a.confirm(yes, fmt.Sprintf("Certify %s (%s), device %s, key %s?", k.UserEmail, k.Role, k.DeviceName, signing.Fingerprint(k.PublicKey)))
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				cert := key.Sign(signing.SignerMessage(cur.ID, k.UserId, k.UserEmail, k.DeviceId, k.PublicKey))
				if _, err := c.CertifySigningKey(ctx, req(s, &workspacev1.CertifySigningKeyRequest{WorkspaceId: cur.ID, DeviceId: k.DeviceId, UserId: k.UserId, Certificate: cert})); err != nil {
					return rpcErr(err)
				}
				done = append(done, k.UserEmail+" "+k.DeviceName)
			}
			if len(done) == 0 {
				return a.done(map[string]any{"certified": []string{}}, "Nothing to certify.\n")
			}
			return a.done(map[string]any{"certified": done}, "✓ Certified: %s. Their signed changes now apply on the team's machines without asking.\n", strings.Join(done, ", "))
		},
	}
	approve.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	cmd.AddCommand(show, approve)
	return cmd
}

func rootWhere(r *workspacev1.TrustRoot) string {
	if r == nil {
		return "the workspace has none yet"
	}
	return fmt.Sprintf("%s's device %s", r.UserEmail, r.DeviceName)
}

func wanted(k *workspacev1.SigningKey, args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, a := range args {
		if strings.EqualFold(a, k.UserEmail) || a == k.DeviceName || a == k.DeviceId {
			return true
		}
	}
	return false
}

func keyRows(res *workspacev1.ListSigningKeysResponse) []signingKeyRow {
	rows := []signingKeyRow{}
	me := credentials.DeviceID()
	for _, k := range res.Keys {
		status := "not certified"
		switch {
		case res.Root != nil && bytes.Equal(res.Root.PublicKey, k.PublicKey):
			status = "root"
		case len(k.Certificate) > 0:
			status = "certified"
		}
		rows = append(rows, signingKeyRow{Email: k.UserEmail, Role: k.Role, Device: k.DeviceName, DeviceID: k.DeviceId,
			Fingerprint: signing.Fingerprint(k.PublicKey), Status: status, ThisDevice: k.DeviceId == me})
	}
	return rows
}

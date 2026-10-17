package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	"github.com/amalychev/devopus/gen/devopus/sync/v1/syncv1connect"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
	"github.com/amalychev/devopus/internal/signing"
)

func TestSigning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.fixture(t)
	userID := func(email string) string { return "usr_" + strings.Split(email, "@")[0] }
	adminKey, _ := signing.NewKey()
	leadKey, _ := signing.NewKey()
	register := func(email, device string, k signing.Key) bool {
		t.Helper()
		return must[workspacev1.RegisterSigningKeyResponse](t)(e.client.RegisterSigningKey(ctx, as(e, email,
			&workspacev1.RegisterSigningKeyRequest{WorkspaceId: f.ws, DeviceId: device, DeviceName: device, PublicKey: k.Public()}))).Certified
	}
	if register(f.admin, "admin-mac", adminKey) {
		t.Error("certified before any root")
	}
	register(f.lead, "lead-mac", leadKey)

	// Only the admin sets the root, once, to a device of their own.
	if _, err := e.client.SetTrustRoot(ctx, as(e, f.lead, &workspacev1.SetTrustRootRequest{WorkspaceId: f.ws, DeviceId: "lead-mac"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("lead sets the root: %v", err)
	}
	if _, err := e.client.SetTrustRoot(ctx, as(e, f.admin, &workspacev1.SetTrustRootRequest{WorkspaceId: f.ws, DeviceId: "lead-mac"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("root on another's device: %v", err)
	}
	must[workspacev1.SetTrustRootResponse](t)(e.client.SetTrustRoot(ctx, as(e, f.admin, &workspacev1.SetTrustRootRequest{WorkspaceId: f.ws, DeviceId: "admin-mac"})))
	if _, err := e.client.SetTrustRoot(ctx, as(e, f.admin, &workspacev1.SetTrustRootRequest{WorkspaceId: f.ws, DeviceId: "admin-mac"})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("a second root: %v", err)
	}
	if !register(f.admin, "admin-mac", adminKey) {
		t.Error("the root's device is not certified")
	}

	// A certificate must be the root's, for this very key.
	bad := leadKey.Sign(signing.SignerMessage(f.ws, userID(f.lead), f.lead, "lead-mac", leadKey.Public()))
	if _, err := e.client.CertifySigningKey(ctx, as(e, f.admin, &workspacev1.CertifySigningKeyRequest{WorkspaceId: f.ws, DeviceId: "lead-mac", UserId: userID(f.lead), Certificate: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a self-made certificate: %v", err)
	}
	cert := adminKey.Sign(signing.SignerMessage(f.ws, userID(f.lead), f.lead, "lead-mac", leadKey.Public()))
	must[workspacev1.CertifySigningKeyResponse](t)(e.client.CertifySigningKey(ctx, as(e, f.admin, &workspacev1.CertifySigningKeyRequest{WorkspaceId: f.ws, DeviceId: "lead-mac", UserId: userID(f.lead), Certificate: cert})))
	keys := must[workspacev1.ListSigningKeysResponse](t)(e.client.ListSigningKeys(ctx, as(e, f.dev, &workspacev1.ListSigningKeysRequest{WorkspaceId: f.ws})))
	if keys.Root == nil || keys.Root.UserEmail != f.admin || len(keys.Keys) != 2 {
		t.Fatalf("keys: %v", keys)
	}

	// The lead changes the project and signs the version; only the author signs it.
	conf := must[workspacev1.PutServiceResponse](t)(e.client.PutService(ctx, as(e, f.lead, &workspacev1.PutServiceRequest{WorkspaceId: f.ws, Project: "shop", Name: "db", ServiceYaml: "type: mysql\nversion: \"8.4\"\n", Create: true}))).Config
	sig := leadKey.Sign(signing.SettingsMessage(f.ws, "shop", conf.Version, []byte(conf.Yaml)))
	if _, err := e.client.SignVersion(ctx, as(e, f.admin, &workspacev1.SignVersionRequest{WorkspaceId: f.ws, Project: "shop", Version: conf.Version, DeviceId: "admin-mac", Signature: adminKey.Sign(signing.SettingsMessage(f.ws, "shop", conf.Version, []byte(conf.Yaml)))})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("another than the author signs: %v", err)
	}
	if _, err := e.client.SignVersion(ctx, as(e, f.lead, &workspacev1.SignVersionRequest{WorkspaceId: f.ws, Project: "shop", Version: conf.Version, DeviceId: "lead-mac", Signature: sig[:10]})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a broken signature: %v", err)
	}
	must[workspacev1.SignVersionResponse](t)(e.client.SignVersion(ctx, as(e, f.lead, &workspacev1.SignVersionRequest{WorkspaceId: f.ws, Project: "shop", Version: conf.Version, DeviceId: "lead-mac", Signature: sig})))
	hist := must[workspacev1.ListProjectVersionsResponse](t)(e.client.ListProjectVersions(ctx, as(e, f.lead, &workspacev1.ListProjectVersionsRequest{WorkspaceId: f.ws, Project: "shop"})))
	if !hist.Versions[0].Signed || hist.Versions[len(hist.Versions)-1].Signed {
		t.Errorf("history: %v", hist.Versions)
	}

	// The developer's agent gets the version with its signature and what to check it
	// with — and checks it.
	sc := syncv1connect.NewSyncServiceClient(http.DefaultClient, e.url)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := sc.Watch(wctx, as(e, f.dev, &syncv1.WatchRequest{WorkspaceId: f.ws, Device: &syncv1.Device{Id: "dev-mac"}}))
	if err != nil || !st.Receive() {
		t.Fatalf("watch: %v %v", err, st.Err())
	}
	m := st.Msg()
	tr := m.Trust
	if tr == nil || tr.RootEmail != f.admin || len(tr.Signers) != 1 {
		t.Fatalf("trust: %v", tr)
	}
	set := signing.Set{Workspace: f.ws, Root: tr.Root, RootDevice: tr.RootDevice, RootEmail: tr.RootEmail}
	for _, s := range tr.Signers {
		set.Signers = append(set.Signers, signing.Signer{DeviceID: s.DeviceId, UserID: s.UserId, Email: s.Email, Public: s.PublicKey, Certificate: s.Certificate})
	}
	p := m.Projects[0]
	if who, err := set.Verify(p.Name, p.Version, []byte(p.ConfigYaml), p.SignerDevice, p.Signature); err != nil || who != f.lead {
		t.Errorf("verify: %q %v", who, err)
	}
}

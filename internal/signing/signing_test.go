package signing

import (
	"errors"
	"testing"
)

func TestVerify(t *testing.T) {
	root, _ := NewKey()
	lead, _ := NewKey()
	stranger, _ := NewKey()
	config := []byte("name: shop\n")
	cert := root.Sign(SignerMessage("wsp_1", "usr_lead", "lead@acme.dev", "lead-mac", lead.Public()))
	set := Set{Workspace: "wsp_1", Root: root.Public(), RootDevice: "admin-mac", RootEmail: "admin@acme.dev",
		Signers: []Signer{
			{DeviceID: "lead-mac", UserID: "usr_lead", Email: "lead@acme.dev", Public: lead.Public(), Certificate: cert},
			// The server adds a device of its own: the certificate is not the root's.
			{DeviceID: "evil", UserID: "usr_lead", Email: "lead@acme.dev", Public: stranger.Public(), Certificate: stranger.Sign([]byte("x"))},
			// Or reuses a real certificate for another key.
			{DeviceID: "lead-mac2", UserID: "usr_lead", Email: "lead@acme.dev", Public: stranger.Public(), Certificate: cert},
		}}

	if who, err := set.Verify("shop", 3, config, "admin-mac", root.Sign(SettingsMessage("wsp_1", "shop", 3, config))); err != nil || who != "admin@acme.dev" {
		t.Errorf("root: %q %v", who, err)
	}
	sig := lead.Sign(SettingsMessage("wsp_1", "shop", 3, config))
	if who, err := set.Verify("shop", 3, config, "lead-mac", sig); err != nil || who != "lead@acme.dev" {
		t.Errorf("certified: %q %v", who, err)
	}
	// The same signature does not fit other settings, another number or project.
	for name, check := range map[string]func() error{
		"other config":  func() error { _, err := set.Verify("shop", 3, []byte("name: shop\nx: 1\n"), "lead-mac", sig); return err },
		"other version": func() error { _, err := set.Verify("shop", 4, config, "lead-mac", sig); return err },
		"other project": func() error { _, err := set.Verify("blog", 3, config, "lead-mac", sig); return err },
	} {
		if err := check(); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, dev := range []string{"evil", "lead-mac2", "nobody"} {
		if _, err := set.Verify("shop", 3, config, dev, stranger.Sign(SettingsMessage("wsp_1", "shop", 3, config))); !errors.Is(err, ErrUntrusted) {
			t.Errorf("%s: %v", dev, err)
		}
	}
	if _, err := set.Verify("shop", 3, config, "", nil); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: %v", err)
	}
	if _, err := (Set{Workspace: "wsp_1"}).Verify("shop", 3, config, "lead-mac", sig); !errors.Is(err, ErrNoRoot) {
		t.Errorf("no root: %v", err)
	}

	k, err := ParseKey(lead.String())
	if err != nil || !k.Public().Equal(lead.Public()) {
		t.Errorf("parse: %v", err)
	}
	if f := Fingerprint(root.Public()); len(f) != 19 || f != Fingerprint(root.Public()) {
		t.Errorf("fingerprint %q", f)
	}
}

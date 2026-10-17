package authz

import (
	"strings"
	"testing"
)

// TestRolesMatchTheTable checks the defaults against docs/product/roles.md, written out
// here by hand: a change of the code must be a change of the table too.
func TestRolesMatchTheTable(t *testing.T) {
	admin := Subject{Role: Admin}
	lead := Subject{Role: Teamlead, Projects: map[string]Role{"shop": Teamlead}}
	dev := Subject{Role: Developer, Projects: map[string]Role{"shop": Developer}}
	stranger := Subject{}

	// permission → who may, in the workspace (w) and in project shop (p).
	table := map[Permission]struct{ w, p string }{
		WorkspaceView:    {"ATD", "ATD"},
		WorkspaceManage:  {"A", "A"},
		ProjectCreate:    {"AT", "A"},
		ProjectConfigure: {"A", "AT"},
		AppsManage:       {"A", "AT"},
		ServicesManage:   {"A", "AT"},
		ServersManage:    {"A", "AT"},
		SecretsManage:    {"A", "AT"},
		SecretsRead:      {"A", "ATD"},
		MembersInvite:    {"AT", "AT"},
		MembersManage:    {"A", "AT"},
		OverridesUse:     {"A", "ATD"},
		EnvUse:           {"A", "ATD"},
		ProductionDeploy: {"A", "A"},
	}
	if len(table) != len(All) {
		t.Fatalf("the table has %d permissions, All has %d", len(table), len(All))
	}
	who := map[byte]Subject{'A': admin, 'T': lead, 'D': dev}
	for p, want := range table {
		for _, c := range []byte("ATD") {
			s := who[c]
			if got := s.Can(p, ""); got != strings.ContainsRune(want.w, rune(c)) {
				t.Errorf("%c %s in the workspace: %v", c, p, got)
			}
			if got := s.Can(p, "shop"); got != strings.ContainsRune(want.p, rune(c)) {
				t.Errorf("%c %s in shop: %v", c, p, got)
			}
			// No role in another project, except the admin's.
			if got := s.Can(p, "other"); got != (c == 'A') {
				t.Errorf("%c %s in other: %v", c, p, got)
			}
		}
		if stranger.Can(p, "") || stranger.Can(p, "shop") {
			t.Errorf("a non-member may %s", p)
		}
	}
}

func TestGrantsAndInvites(t *testing.T) {
	lead := Subject{Role: Teamlead, Projects: map[string]Role{"shop": Teamlead, "blog": Developer}}
	dev := Subject{Role: Developer, Projects: map[string]Role{"shop": Developer}, Grants: map[string][]Permission{"shop": {ServicesManage}}}

	if !dev.Can(ServicesManage, "shop") || dev.Can(AppsManage, "shop") {
		t.Error("grant")
	}
	if err := lead.CanGrant(ServicesManage, "shop"); err != nil {
		t.Error(err)
	}
	if err := lead.CanGrant(ServicesManage, "blog"); err == nil {
		t.Error("a developer of blog grants there")
	}
	if err := lead.CanGrant(MembersManage, "shop"); err == nil {
		t.Error("members.manage is not grantable")
	}
	if err := dev.CanGrant(ServicesManage, "shop"); err == nil {
		t.Error("a developer grants")
	}

	if err := lead.CanInvite(Developer, []string{"shop"}); err != nil {
		t.Error(err)
	}
	for _, c := range []struct {
		role     Role
		projects []string
	}{{Teamlead, []string{"shop"}}, {Developer, nil}, {Developer, []string{"shop", "blog"}}} {
		if err := lead.CanInvite(c.role, c.projects); err == nil {
			t.Errorf("teamlead invites %s into %v", c.role, c.projects)
		}
	}
	if err := (Subject{Role: Admin}).CanInvite(Teamlead, nil); err != nil {
		t.Error(err)
	}
	if err := dev.CanInvite(Developer, []string{"shop"}); err == nil {
		t.Error("a developer invites")
	}
	if _, err := ParseRole("admin"); err == nil {
		t.Error("admin by invite")
	}
}

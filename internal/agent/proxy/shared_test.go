package proxy

import (
	"net/netip"
	"strings"
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/amalychev/devopus/internal/agent/pki"
)

func target(name string, port uint32, ip string) SharedTarget {
	return SharedTarget{Name: name, Port: port, Addr: netip.AddrPortFrom(netip.MustParseAddr(ip), uint16(port))}
}

func TestSharedCheck(t *testing.T) {
	ok := &Shared{IP: netip.MustParseAddr("10.213.5.254"),
		Apps:     []SharedTarget{target("api", 8080, "10.213.4.130"), target("auth", 8080, "10.213.4.131"), target("legacy", 80, "10.213.4.132")},
		Services: []SharedTarget{target("db", 5432, "10.213.4.140"), target("admin", 8443, "10.213.4.141")},
	}
	if err := ok.Check(18080, 18443); err != nil {
		t.Errorf("applications sharing a port, an application on 80 and a service on the inner HTTPS port: %v", err)
	}
	var nilShared *Shared
	if err := nilShared.Check(80, 443); err != nil {
		t.Error(err)
	}
	for name, sh := range map[string]*Shared{
		"application on 443":          {Apps: []SharedTarget{target("api", 443, "10.0.0.1")}},
		"application on public HTTPS": {Apps: []SharedTarget{target("api", 18443, "10.0.0.1")}},
		"service on 80":               {Services: []SharedTarget{target("db", 80, "10.0.0.1")}},
		"two services":                {Services: []SharedTarget{target("db", 5432, "10.0.0.1"), target("db2", 5432, "10.0.0.2")}},
		"application and service":     {Apps: []SharedTarget{target("api", 9000, "10.0.0.1")}, Services: []SharedTarget{target("s3", 9000, "10.0.0.2")}},
	} {
		if err := sh.Check(18080, 18443); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSharedResources(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, _ := ca.Issue([]string{"shop.localhost"})
	feat, _ := ca.Issue([]string{"pay-1.shop.localhost"})
	other, _ := ca.Issue([]string{"pay-2.shop.localhost"})
	shared := func(ip string) *Shared {
		return &Shared{IP: netip.MustParseAddr(ip),
			Apps:     []SharedTarget{target("api", 8080, "10.213.4.130"), target("auth", 3000, "10.213.4.131"), target("legacy", 80, "10.213.4.132")},
			Services: []SharedTarget{target("db", 5432, "10.213.4.140")},
		}
	}
	cfg := Config{HTTPPort: 80, HTTPSPort: 443, Sites: []Site{
		{Name: "base", Hosts: base.Hosts, CertPEM: base.CertPEM, KeyPEM: base.KeyPEM},
		{Name: "f1", Hosts: feat.Hosts, CertPEM: feat.CertPEM, KeyPEM: feat.KeyPEM, Shared: shared("10.213.5.254")},
		{Name: "f2", Hosts: other.Hosts, CertPEM: other.CertPEM, KeyPEM: other.KeyPEM, Shared: shared("10.213.6.254")},
	}}
	res, _ := consistent(t, cfg)
	byName := map[string]*listenerv3.Listener{}
	for _, r := range res[resource.ListenerType] {
		l := r.(*listenerv3.Listener)
		byName[l.Name] = l
	}
	// 8080 and 80 belong to the HTTP listener: two features, a chain each per port, by destination.
	http := byName["http-80"]
	if http == nil || len(http.FilterChains) != 5 {
		t.Fatalf("http listener: %v", http)
	}
	for _, fc := range http.FilterChains[1:] {
		m := fc.FilterChainMatch
		if m == nil || len(m.PrefixRanges) != 1 || m.DestinationPort == nil {
			t.Errorf("chain %s matches %v", fc.Name, m)
		}
	}
	// Other ports get a listener each, with both features' chains.
	for _, name := range []string{"shared-3000", "shared-5432"} {
		if l := byName[name]; l == nil || len(l.FilterChains) != 2 {
			t.Errorf("%s: %v", name, l)
		}
	}
	// The HTTP chain on port 80 still redirects the environments' hosts.
	found := false
	for _, r := range res[resource.RouteType] {
		if rc := r.(*routev3.RouteConfiguration); rc.Name == "shared-f1-80" {
			s := prototext.Format(rc)
			found = strings.Contains(s, "shop.localhost") && strings.Contains(s, "legacy")
		}
	}
	if !found {
		t.Error("the shared route table on port 80 lacks the environments' hosts")
	}
}

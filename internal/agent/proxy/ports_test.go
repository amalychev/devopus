package proxy

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestParseLsof(t *testing.T) {
	out := "p569\ncnginx\nn*:80\np1200\ncpython3\nn127.0.0.1:80\n"
	if name, pid := parseLsof(out, netip.MustParseAddrPort("127.0.0.1:80")); name != "python3" || pid != 1200 {
		t.Errorf("exact: %s %d", name, pid)
	}
	if name, pid := parseLsof(out, netip.MustParseAddrPort("[::1]:80")); name != "nginx" || pid != 569 {
		t.Errorf("wildcard: %s %d", name, pid)
	}
	if name, _ := parseLsof("", netip.MustParseAddrPort("[::1]:80")); name != "" {
		t.Errorf("empty: %s", name)
	}
}

func TestParseSS(t *testing.T) {
	name, pid := parseSS(`LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=812,fd=6),("nginx",pid=813,fd=6))`)
	if name != "nginx" || pid != 812 {
		t.Errorf("%s %d", name, pid)
	}
}

func TestTakenFindsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	got := Taken(context.Background(), Ports{HTTP: port, HTTPS: port})
	if len(got) == 0 || got[0].Addr.Port() != port {
		t.Fatalf("conflicts: %v", got)
	}
	// lsof sees our own process.
	if got[0].Owner == "" || !strings.Contains(got[0].String(), "taken by") {
		t.Errorf("owner: %+v", got[0])
	}
	if kindOf("nginx") != "nginx" || kindOf("com.docker.backend") != "runtime" || kindOf("python3") != "" {
		t.Error("kindOf")
	}
}

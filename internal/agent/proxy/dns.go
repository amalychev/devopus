package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/devopus/devopus/internal/agent/runtime"
)

// DNSImage answers *.localhost inside environment networks, pinned by digest.
const DNSImage = "coredns/coredns:1.14.7@sha256:7efd3c635b03efd68c4e8398fc45f0d993d0e9ab016f72c1cefb0fd6d01aa286"

// Fixed addresses in every environment network (the upper half of the /24, which
// Docker does not hand out): the proxy and the DNS server.
func ProxyIP(subnet netip.Prefix) netip.Addr { return hostAddr(subnet, 254) }
func DNSIP(subnet netip.Prefix) netip.Addr   { return hostAddr(subnet, 253) }

func hostAddr(subnet netip.Prefix, last byte) netip.Addr {
	a := subnet.Masked().Addr().As4()
	a[3] = last
	return netip.AddrFrom4(a)
}

// Corefile makes *.localhost resolve to the proxy's address in this network; everything
// else goes to Docker's embedded DNS of the DNS container, which knows only this
// network, so names of other environments never leak in (local-networking.md §2, P-6).
// In a feature the names it shares with the base environment are the proxy too.
func Corefile(proxyIP netip.Addr, shared []string) string {
	hosts := ""
	if len(shared) > 0 {
		names := append([]string{}, shared...)
		sort.Strings(names)
		hosts = fmt.Sprintf(`    # Modules and services of the base environment: the proxy forwards to them.
    hosts {
        %s %s
        ttl 60
        no_reverse
        fallthrough
    }
`, proxyIP, strings.Join(names, " "))
	}
	return fmt.Sprintf(`.:53 {
    errors
%s    # Every name under localhost is the proxy. No AAAA on purpose: clients must not
    # try ::1, which inside a container is the container itself.
    template IN A localhost {
        answer "{{ .Name }} 60 IN A %s"
    }
    template IN AAAA localhost {
        rcode NOERROR
    }
    forward . 127.0.0.11
    cache 30
}
`, hosts, proxyIP)
}

// DNSSpec describes the DNS container of an environment network.
func DNSSpec(name, network string, subnet netip.Prefix, labels map[string]string, shared []string) runtime.ContainerSpec {
	corefile := Corefile(ProxyIP(subnet), shared)
	sum := sha256.Sum256([]byte(DNSImage + "\n" + corefile + "\nmac-from-ip"))
	all := map[string]string{}
	for k, v := range labels {
		all[k] = v
	}
	all[runtime.LabelRole] = "dns"
	all[runtime.LabelConfig] = hex.EncodeToString(sum[:])[:16]
	return runtime.ContainerSpec{
		Name:    name,
		Image:   DNSImage,
		Cmd:     []string{"-conf", "/etc/coredns/Corefile"},
		Labels:  all,
		Network: network,
		IP:      DNSIP(subnet),
		Files:   []runtime.File{{Path: "/etc/coredns/Corefile", Data: []byte(corefile), Mode: 0o644}},
		Restart: "unless-stopped",
	}
}

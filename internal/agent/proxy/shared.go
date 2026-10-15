package proxy

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Shared is how a feature reaches modules and services of its base environment by
// their logical names (Simple Isolation, P-2): the feature's DNS answers those names
// with the proxy's address in the feature network, and the proxy tells the feature
// and the port apart by the destination of the connection. The base environment's
// containers never join the feature network, so their own calls stay in the base.
type Shared struct {
	// IP is the proxy's address in the feature network.
	IP netip.Addr
	// Modules are reached over HTTP, told apart by Host: several may share a port.
	Modules []SharedTarget
	// Services are reached over TCP: one per port.
	Services []SharedTarget
}

// SharedTarget is a module or service of the base environment.
type SharedTarget struct {
	Name string
	// Port is what the feature connects to: the same as in the base environment, so
	// ${{ <name>.url }} and <name>:<port> do not change.
	Port uint32
	// Addr is the container in the base environment network.
	Addr netip.AddrPort
}

// Check reports what the proxy cannot carry for a feature: a port it needs for the
// environment's own URL, or two services on one port.
func (s *Shared) Check(httpPort, httpsPort uint32) error {
	if s == nil {
		return nil
	}
	reserved := map[uint32]string{443: "HTTPS", httpsPort: "HTTPS"}
	byPort := map[uint32][]string{}
	for _, m := range s.Modules {
		if what, ok := reserved[m.Port]; ok {
			return fmt.Errorf("module %s listens on port %d, which the proxy needs for %s in the feature; fork %s too", m.Name, m.Port, what, m.Name)
		}
	}
	reserved[80], reserved[httpPort] = "HTTP", "HTTP"
	for _, sv := range s.Services {
		if what, ok := reserved[sv.Port]; ok {
			return fmt.Errorf("service %s listens on port %d, which the proxy needs for %s in the feature; fork a module that uses it", sv.Name, sv.Port, what)
		}
		byPort[sv.Port] = append(byPort[sv.Port], sv.Name)
	}
	for _, m := range s.Modules {
		if names, ok := byPort[m.Port]; ok {
			return fmt.Errorf("module %s and service %s of the base environment both listen on port %d; fork %s to run it in the feature", m.Name, names[0], m.Port, m.Name)
		}
	}
	for port, names := range byPort {
		if len(names) > 1 {
			sort.Strings(names)
			return fmt.Errorf("services %s of the base environment all listen on port %d; the feature can share only one of them", strings.Join(names, ", "), port)
		}
	}
	return nil
}

// sharedChains are the filter chains of the shared modules and services of every
// site, by port, with the route tables and clusters they need. HTTP chains on the
// ports where the environments' URLs are (httpPorts) also serve those hosts.
func sharedChains(sites []Site, httpVHosts []*routev3.VirtualHost, httpPorts map[uint32]bool) (map[uint32][]*listenerv3.FilterChain, []types.Resource, []types.Resource, error) {
	chains := map[uint32][]*listenerv3.FilterChain{}
	var routes, clusters []types.Resource
	seen := map[string]bool{}
	cluster := func(site string, t SharedTarget) string {
		name := fmt.Sprintf("%s-shared-%s-%d", site, t.Name, t.Port)
		if !seen[name] {
			seen[name] = true
			clusters = append(clusters, staticCluster(name, t.Addr))
		}
		return name
	}
	for _, s := range sites {
		sh := s.Shared
		if sh == nil || !sh.IP.IsValid() {
			continue
		}
		match := func(port uint32) *listenerv3.FilterChainMatch {
			return &listenerv3.FilterChainMatch{
				DestinationPort: wrapperspb.UInt32(port),
				PrefixRanges:    []*corev3.CidrRange{{AddressPrefix: sh.IP.String(), PrefixLen: wrapperspb.UInt32(32)}},
			}
		}
		modules := map[uint32][]SharedTarget{}
		for _, m := range sh.Modules {
			modules[m.Port] = append(modules[m.Port], m)
		}
		ports := make([]uint32, 0, len(modules))
		for p := range modules {
			ports = append(ports, p)
		}
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		for _, port := range ports {
			name := fmt.Sprintf("shared-%s-%d", s.Name, port)
			var vhosts []*routev3.VirtualHost
			var names []string
			for _, m := range modules[port] {
				names = append(names, m.Name)
				vhosts = append(vhosts, &routev3.VirtualHost{
					Name:    m.Name,
					Domains: []string{m.Name},
					Routes: []*routev3.Route{{
						Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster(s.Name, m)}, Timeout: durationpb.New(0)}},
					}},
				})
			}
			if httpPorts[port] {
				vhosts = append(vhosts, httpVHosts...)
			} else {
				vhosts = append(vhosts, &routev3.VirtualHost{Name: "default", Domains: []string{"*"}, Routes: []*routev3.Route{pageRoute("/", Page{
					Status: 404, Title: "No such module",
					Text: fmt.Sprintf("On port %d the base environment shares %s.", port, strings.Join(names, ", ")),
				})}})
			}
			routes = append(routes, &routev3.RouteConfiguration{Name: name, VirtualHosts: vhosts})
			hcm, err := httpManager(name, name)
			if err != nil {
				return nil, nil, nil, err
			}
			chains[port] = append(chains[port], &listenerv3.FilterChain{Name: name, FilterChainMatch: match(port), Filters: []*listenerv3.Filter{hcm}})
		}
		for _, sv := range sh.Services {
			name := fmt.Sprintf("shared-%s-%d", s.Name, sv.Port)
			tcp, err := anypb.New(&tcpproxyv3.TcpProxy{StatPrefix: name, ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: cluster(s.Name, sv)}})
			if err != nil {
				return nil, nil, nil, err
			}
			chains[sv.Port] = append(chains[sv.Port], &listenerv3.FilterChain{
				Name:             name,
				FilterChainMatch: match(sv.Port),
				Filters:          []*listenerv3.Filter{{Name: "envoy.filters.network.tcp_proxy", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tcp}}},
			})
		}
	}
	return chains, routes, clusters, nil
}

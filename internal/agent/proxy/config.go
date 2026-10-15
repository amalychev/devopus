// Package proxy runs the device's single Envoy (ADR-0004): it builds Envoy's dynamic
// configuration from the environments, serves it over xDS/SDS, and describes the
// Envoy container. Nothing here restarts Envoy to change routes (P-1).
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/stream/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	tlsinspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Ports Envoy listens on inside its container; the runtime publishes them on the
// host's loopback as 80/443 or the fallback ports.
const (
	ContainerHTTPPort  = 8080
	ContainerHTTPSPort = 8443
)

// Names of the dynamic resources.
const (
	listenerHTTP  = "http"
	listenerHTTPS = "https"
	routesHTTP    = "http"
	routesHTTPS   = "https"
	clusterCoHTTP = "coexist-http"
	clusterCoTLS  = "coexist-https"
)

// HostGateway is how a container reaches the host; on Linux the agent maps it to
// host-gateway when it creates the container.
const HostGateway = "host.docker.internal"

// Config is everything Envoy serves.
type Config struct {
	// HTTPPort and HTTPSPort are the public ports; HTTP redirects to HTTPS. Envoy also
	// listens on them inside the environment networks, so a module that calls its
	// environment's URL reaches the proxy on the same port as a browser does.
	HTTPPort  uint32
	HTTPSPort uint32
	Sites     []Site
	// Coexist sends hosts Devopus does not serve to a dev server moved off 80/443.
	Coexist *Coexist
}

// Coexist is a dev server on the host (Herd, Valet, nginx) that gave 80/443 to Devopus.
type Coexist struct {
	HTTPPort  uint32
	HTTPSPort uint32 // 0: TLS for foreign hosts is not passed through
}

// Site is one environment: its hosts share a certificate and a route table.
type Site struct {
	// Name identifies the site in resource names: the environment ID.
	Name    string
	Project string
	Hosts   []string
	CertPEM []byte
	KeyPEM  []byte
	// VHosts route the hosts to modules and service web UIs. A site without them shows
	// Page on every host and path: before modules run, and while the environment is stopped.
	VHosts []VHost
	Page   Page
	// Shared: a feature's way to the base environment's modules and services.
	Shared *Shared
}

// VHost is a set of hosts with one route table.
type VHost struct {
	Hosts  []string
	Routes []PathRoute
	// NotFound answers paths no route takes.
	NotFound Page
}

// PathRoute sends a path prefix to an upstream: "/api" takes /api and /api/…, not /apix.
type PathRoute struct {
	Prefix   string
	Upstream Upstream
}

// Upstream is a container in the environment network, at its pinned address.
type Upstream struct {
	// Name is unique within the site: the module or service.
	Name string
	Addr netip.AddrPort
}

// Page is a direct response of the proxy.
type Page struct {
	Status uint32
	Title  string
	Text   string
}

// Resources builds Envoy's resources and a version that changes only with them.
func (c Config) Resources() (map[resource.Type][]types.Resource, string, error) {
	sites := append([]Site{}, c.Sites...)
	sort.Slice(sites, func(i, j int) bool { return sites[i].Name < sites[j].Name })

	var secrets, clusters []types.Resource
	var chains []*listenerv3.FilterChain
	httpsVHosts := []*routev3.VirtualHost{}
	httpVHosts := []*routev3.VirtualHost{}
	var known []string
	for _, s := range sites {
		if len(s.Hosts) == 0 {
			continue
		}
		secret := "cert-" + s.Name
		secrets = append(secrets, &tlsv3.Secret{
			Name: secret,
			Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: inlineBytes(s.CertPEM),
				PrivateKey:       inlineBytes(s.KeyPEM),
			}},
		})
		hcm, err := httpManager(listenerHTTPS+"-"+s.Name, routesHTTPS)
		if err != nil {
			return nil, "", err
		}
		tls, err := anypb.New(&tlsv3.DownstreamTlsContext{CommonTlsContext: &tlsv3.CommonTlsContext{
			AlpnProtocols:                  []string{"h2", "http/1.1"},
			TlsCertificateSdsSecretConfigs: []*tlsv3.SdsSecretConfig{{Name: secret, SdsConfig: ads()}},
			TlsParams:                      &tlsv3.TlsParameters{TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_2},
		}})
		if err != nil {
			return nil, "", err
		}
		chains = append(chains, &listenerv3.FilterChain{
			Name:             s.Name,
			FilterChainMatch: &listenerv3.FilterChainMatch{ServerNames: s.Hosts},
			Filters:          []*listenerv3.Filter{hcm},
			TransportSocket:  &corev3.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: tls}},
		})
		if len(s.VHosts) == 0 {
			httpsVHosts = append(httpsVHosts, &routev3.VirtualHost{
				Name:    s.Name,
				Domains: s.Hosts,
				Routes:  []*routev3.Route{healthRoute(), pageRoute("/", s.Page)},
			})
		}
		upstreams := map[string]bool{}
		for i, vh := range s.VHosts {
			v := &routev3.VirtualHost{Name: fmt.Sprintf("%s-%d", s.Name, i), Domains: vh.Hosts, Routes: []*routev3.Route{healthRoute()}}
			routes := append([]PathRoute{}, vh.Routes...)
			// Envoy takes the first match: longer prefixes first.
			sort.SliceStable(routes, func(i, j int) bool { return len(routes[i].Prefix) > len(routes[j].Prefix) })
			for _, rt := range routes {
				cluster := s.Name + "-" + rt.Upstream.Name
				if !upstreams[cluster] {
					upstreams[cluster] = true
					clusters = append(clusters, staticCluster(cluster, rt.Upstream.Addr))
				}
				v.Routes = append(v.Routes, &routev3.Route{
					Match: prefixMatch(rt.Prefix),
					// No timeout: dev servers compile on the first request, streams stay open.
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}, Timeout: durationpb.New(0)}},
				})
			}
			if !hasRoot(routes) {
				v.Routes = append(v.Routes, pageRoute("/", vh.NotFound))
			}
			httpsVHosts = append(httpsVHosts, v)
		}
		redirect := &routev3.RedirectAction{SchemeRewriteSpecifier: &routev3.RedirectAction_HttpsRedirect{HttpsRedirect: true}}
		if c.HTTPSPort != 443 {
			redirect.PortRedirect = c.HTTPSPort
		}
		httpVHosts = append(httpVHosts, &routev3.VirtualHost{
			Name:    s.Name,
			Domains: s.Hosts,
			Routes: []*routev3.Route{{
				Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: &routev3.Route_Redirect{Redirect: redirect},
			}},
		})
		known = append(known, s.Hosts...)
	}

	unknown := Page{Status: 404, Title: "Nothing here", Text: unknownText(known)}
	httpDefault := &routev3.VirtualHost{Name: "default", Domains: []string{"*"}, Routes: []*routev3.Route{pageRoute("/", unknown)}}
	if c.Coexist != nil && c.Coexist.HTTPPort != 0 {
		clusters = append(clusters, hostCluster(clusterCoHTTP, c.Coexist.HTTPPort))
		httpDefault.Routes = []*routev3.Route{{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: clusterCoHTTP}, Timeout: durationpb.New(0)}},
		}}
	}
	httpVHosts = append(httpVHosts, httpDefault)
	httpsVHosts = append(httpsVHosts, &routev3.VirtualHost{Name: "default", Domains: []string{"*"}, Routes: []*routev3.Route{pageRoute("/", unknown)}})

	httpHCM, err := httpManager(listenerHTTP, routesHTTP)
	if err != nil {
		return nil, "", err
	}
	httpPort, httpsPort := c.HTTPPort, c.HTTPSPort
	if httpPort == 0 {
		httpPort = 80
	}
	if httpsPort == 0 {
		httpsPort = 443
	}
	shared, sharedRoutes, sharedClusters, err := sharedChains(sites, httpVHosts, map[uint32]bool{80: true, httpPort: true})
	if err != nil {
		return nil, "", err
	}
	clusters = append(clusters, sharedClusters...)
	// Shared ports that the HTTP and HTTPS listeners hold go into them, told apart by
	// the destination; the others get a listener each.
	take := func(ports ...uint32) []*listenerv3.FilterChain {
		var out []*listenerv3.FilterChain
		for _, p := range ports {
			out = append(out, shared[p]...)
			delete(shared, p)
		}
		return out
	}
	httpListener := &listenerv3.Listener{
		// The name carries the ports: Envoy does not change a listener's addresses in place.
		Name:                listenerHTTP + "-" + fmt.Sprint(httpPort),
		Address:             anyAddress(ContainerHTTPPort),
		AdditionalAddresses: additional(ContainerHTTPPort, 80, httpPort),
		FilterChains:        append([]*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{httpHCM}}}, take(uniq(ContainerHTTPPort, 80, httpPort)...)...),
	}
	listeners := []types.Resource{httpListener}
	inspector, err := anypb.New(&tlsinspectorv3.TlsInspector{})
	if err != nil {
		return nil, "", err
	}
	https := &listenerv3.Listener{
		Name:                listenerHTTPS + "-" + fmt.Sprint(httpsPort),
		Address:             anyAddress(ContainerHTTPSPort),
		AdditionalAddresses: additional(ContainerHTTPSPort, 443, httpsPort),
		ListenerFilters:     []*listenerv3.ListenerFilter{{Name: "envoy.filters.listener.tls_inspector", ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: inspector}}},
		FilterChains:        chains,
	}
	if c.Coexist != nil && c.Coexist.HTTPSPort != 0 {
		clusters = append(clusters, hostCluster(clusterCoTLS, c.Coexist.HTTPSPort))
		tcp, err := anypb.New(&tcpproxyv3.TcpProxy{StatPrefix: clusterCoTLS, ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: clusterCoTLS}})
		if err != nil {
			return nil, "", err
		}
		https.DefaultFilterChain = &listenerv3.FilterChain{Name: "coexist", Filters: []*listenerv3.Filter{{Name: "envoy.filters.network.tcp_proxy", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tcp}}}}
	}
	// An HTTPS listener without chains refuses every connection, which is what we want
	// before the first environment, but Envoy rejects a listener with no chain at all.
	routes := []types.Resource{&routev3.RouteConfiguration{Name: routesHTTP, VirtualHosts: httpVHosts}}
	siteChains := len(https.FilterChains) > 0
	https.FilterChains = append(https.FilterChains, take(uniq(ContainerHTTPSPort, 443, httpsPort)...)...)
	if len(https.FilterChains) > 0 || https.DefaultFilterChain != nil {
		listeners = append(listeners, https)
		// Only a listener that references it may bring it: the snapshot must be consistent.
		if siteChains {
			routes = append(routes, &routev3.RouteConfiguration{Name: routesHTTPS, VirtualHosts: httpsVHosts})
		}
	}
	routes = append(routes, sharedRoutes...)
	ports := make([]uint32, 0, len(shared))
	for p := range shared {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	for _, p := range ports {
		listeners = append(listeners, &listenerv3.Listener{
			Name:         fmt.Sprintf("shared-%d", p),
			Address:      anyAddress(p),
			FilterChains: shared[p],
		})
	}

	res := map[resource.Type][]types.Resource{
		resource.ListenerType: listeners,
		resource.RouteType:    routes,
		resource.ClusterType:  clusters,
		resource.SecretType:   secrets,
	}
	version, err := hashResources(res)
	return res, version, err
}

func httpManager(stat, routes string) (*listenerv3.Filter, error) {
	router, err := anypb.New(&routerv3.Router{})
	if err != nil {
		return nil, err
	}
	stdout, err := anypb.New(&streamv3.StdoutAccessLog{})
	if err != nil {
		return nil, err
	}
	m, err := anypb.New(&hcmv3.HttpConnectionManager{
		StatPrefix: stat,
		CodecType:  hcmv3.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcmv3.HttpConnectionManager_Rds{Rds: &hcmv3.Rds{
			RouteConfigName: routes,
			ConfigSource:    ads(),
		}},
		HttpFilters:   []*hcmv3.HttpFilter{{Name: "envoy.filters.http.router", ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: router}}},
		AccessLog:     []*accesslogv3.AccessLog{{Name: "envoy.access_loggers.stdout", ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: stdout}}},
		ServerName:    "devopus",
		NormalizePath: wrapperspb.Bool(true),
		MergeSlashes:  true,
		// Hosts match with or without the fallback port.
		StripPortMode: &hcmv3.HttpConnectionManager_StripAnyHostPort{StripAnyHostPort: true},
		// Upgrades for dev servers: Vite, webpack and Next.js reload over websockets.
		UpgradeConfigs: []*hcmv3.HttpConnectionManager_UpgradeConfig{{UpgradeType: "websocket"}},
	})
	if err != nil {
		return nil, err
	}
	return &listenerv3.Filter{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: m}}, nil
}

// HealthPath is answered by the proxy itself on every site: the agent checks with it
// that a site is served, without waiting for the modules behind it.
const HealthPath = "/.devopus/health"

func healthRoute() *routev3.Route {
	return &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: HealthPath}},
		Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 204}},
	}
}

func prefixMatch(prefix string) *routev3.RouteMatch {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}}
	}
	return &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_PathSeparatedPrefix{PathSeparatedPrefix: prefix}}
}

func hasRoot(routes []PathRoute) bool {
	for _, r := range routes {
		if strings.TrimSuffix(r.Prefix, "/") == "" {
			return true
		}
	}
	return false
}

// staticCluster reaches a container at its pinned address in the environment network.
func staticCluster(name string, addr netip.AddrPort) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 name,
		ConnectTimeout:       durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(addr.Addr().String(), uint32(addr.Port()))}},
			}}}},
		},
	}
}

func pageRoute(prefix string, p Page) *routev3.Route {
	return &routev3.Route{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: prefix}},
		Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
			Status: p.Status,
			Body:   inlineString(renderPage(p)),
		}},
		ResponseHeadersToAdd: []*corev3.HeaderValueOption{{
			Header:       &corev3.HeaderValue{Key: "content-type", Value: "text/html; charset=utf-8"},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}},
	}
}

func renderPage(p Page) string {
	return "<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'>" +
		"<title>" + html.EscapeString(p.Title) + " · Devopus</title>" +
		"<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222;background:#fff}" +
		"@media(prefers-color-scheme:dark){body{color:#ddd;background:#16171a}}code{font-size:.95em}</style>" +
		"<h1>" + html.EscapeString(p.Title) + "</h1><p>" + p.Text + "</p></html>"
}

func unknownText(known []string) string {
	if len(known) == 0 {
		return "Devopus serves no environments yet. Run <code>devopus up</code> in a project."
	}
	sort.Strings(known)
	var b strings.Builder
	b.WriteString("Devopus has no environment at this address. It serves:</p><ul>")
	for _, h := range known {
		fmt.Fprintf(&b, "<li><a href='https://%s/'>%s</a></li>", html.EscapeString(h), html.EscapeString(h))
	}
	b.WriteString("</ul><p>")
	return b.String()
}

// hostCluster reaches a port on the host from inside the Envoy container.
func hostCluster(name string, port uint32) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 name,
		ConnectTimeout:       durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS},
		DnsLookupFamily:      clusterv3.Cluster_V4_ONLY,
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(HostGateway, port)}},
			}}}},
		},
	}
}

func ads() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ResourceApiVersion:    corev3.ApiVersion_V3,
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
	}
}

func anyAddress(port uint32) *corev3.Address { return socketAddress("0.0.0.0", port) }

// additional are the ports a listener takes besides the published one: the standard
// port and the public one, for clients inside environment networks.
func additional(published uint32, ports ...uint32) []*listenerv3.AdditionalAddress {
	seen := map[uint32]bool{published: true}
	var out []*listenerv3.AdditionalAddress
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, &listenerv3.AdditionalAddress{Address: anyAddress(p)})
		}
	}
	return out
}

func uniq(ports ...uint32) []uint32 {
	var out []uint32
	for _, p := range ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func socketAddress(host string, port uint32) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Address:       host,
		PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
	}}}
}

func inlineBytes(b []byte) *corev3.DataSource {
	return &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: b}}
}

func inlineString(s string) *corev3.DataSource {
	return &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: s}}
}

// hashResources is a content hash, so an agent restart that rebuilds the same
// configuration presents the same version and Envoy changes nothing.
func hashResources(res map[resource.Type][]types.Resource) (string, error) {
	h := sha256.New()
	typs := make([]string, 0, len(res))
	for t := range res {
		typs = append(typs, t)
	}
	sort.Strings(typs)
	opt := proto.MarshalOptions{Deterministic: true}
	for _, t := range typs {
		h.Write([]byte(t))
		for _, r := range res[t] {
			b, err := opt.Marshal(r)
			if err != nil {
				return "", err
			}
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

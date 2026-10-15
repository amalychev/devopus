package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/devopus/devopus/internal/agent/pki"
	"github.com/devopus/devopus/internal/agent/runtime"
)

// Image is the Envoy the agent runs, pinned by digest: a tag can move under us.
const Image = "envoyproxy/envoy:v1.39.4@sha256:74bd4eb5629708d3fadca1a5e6b85d208f338d7bb54e06b69565905c5294cc01"

// envoyUID is the user the image's entrypoint drops to; the bootstrap holds a key, so
// it is readable by that user only.
const envoyUID = 101

const bootstrapPath = "/etc/devopus/envoy.json"

// Ports are the host ports the proxy is published on.
type Ports struct {
	HTTP  uint16
	HTTPS uint16
}

var (
	DefaultPorts  = Ports{HTTP: 80, HTTPS: 443}
	FallbackPorts = Ports{HTTP: 18080, HTTPS: 18443}
)

// Mode names the port mode for people (local-networking.md §1).
func (p Ports) Mode(coexist bool) string {
	switch {
	case p != DefaultPorts:
		return "fallback"
	case coexist:
		return "coexist"
	}
	return "default"
}

// Loopback is where the proxy is published: both families, never 0.0.0.0. Browsers
// and most clients try ::1 first (P-6), so listening on IPv4 alone lets anyone on ::1
// take the traffic.
var Loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}

// ContainerSpec describes the Envoy container. Its config label changes whenever the
// container must be recreated: another image, bootstrap or ports.
func ContainerSpec(name string, labels map[string]string, ports Ports, xdsPort int, ctl *pki.Control) (runtime.ContainerSpec, error) {
	boot, err := Bootstrap(xdsPort, ctl)
	if err != nil {
		return runtime.ContainerSpec{}, err
	}
	var bind []runtime.PortBinding
	for _, ip := range Loopback {
		bind = append(bind,
			runtime.PortBinding{HostIP: ip, HostPort: ports.HTTP, ContainerPort: ContainerHTTPPort},
			runtime.PortBinding{HostIP: ip, HostPort: ports.HTTPS, ContainerPort: ContainerHTTPSPort},
		)
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\n%d/%d\nunprivileged-ports\n", Image, ports.HTTP, ports.HTTPS)
	sum.Write(boot)
	all := map[string]string{}
	for k, v := range labels {
		all[k] = v
	}
	all[runtime.LabelRole] = "proxy"
	all[runtime.LabelConfig] = hex.EncodeToString(sum.Sum(nil))[:16]
	all[LabelHTTPPort] = fmt.Sprint(ports.HTTP)
	all[LabelHTTPSPort] = fmt.Sprint(ports.HTTPS)
	return runtime.ContainerSpec{
		Name:   name,
		Image:  Image,
		Cmd:    []string{"-c", bootstrapPath, "--log-level", "warning"},
		Labels: all,
		Ports:  bind,
		// Docker Desktop, OrbStack and Colima define host.docker.internal; Linux needs it spelled out.
		ExtraHosts: []string{HostGateway + ":host-gateway"},
		Files:      []runtime.File{{Path: bootstrapPath, Data: boot, Mode: 0o600, UID: envoyUID, GID: envoyUID}},
		Restart:    "unless-stopped",
		// Envoy runs as a user and also listens on 80/443 inside environment networks.
		Sysctls: map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
	}, nil
}

// Labels with the published ports, so an agent restart knows the mode without probing.
const (
	LabelHTTPPort  = "com.devopus.proxy.http-port"
	LabelHTTPSPort = "com.devopus.proxy.https-port"
)

// PortsOf reads the published ports back from a proxy container's labels.
func PortsOf(labels map[string]string) (Ports, bool) {
	var p Ports
	if _, err := fmt.Sscan(labels[LabelHTTPPort], &p.HTTP); err != nil {
		return p, false
	}
	if _, err := fmt.Sscan(labels[LabelHTTPSPort], &p.HTTPS); err != nil {
		return p, false
	}
	return p, true
}

// Bootstrap is Envoy's static configuration: who it is and where its xDS server is.
// Everything else comes over ADS.
func Bootstrap(xdsPort int, ctl *pki.Control) ([]byte, error) {
	upstreamTLS, err := anypb.New(&tlsv3.UpstreamTlsContext{
		Sni: pki.ServerName,
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsCertificates: []*tlsv3.TlsCertificate{{
				CertificateChain: inlineBytes(ctl.ClientPEM),
				PrivateKey:       inlineBytes(ctl.ClientKey),
			}},
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{ValidationContext: &tlsv3.CertificateValidationContext{
				TrustedCa: inlineBytes(ctl.CAPEM),
				MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
					SanType: tlsv3.SubjectAltNameMatcher_DNS,
					Matcher: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: pki.ServerName}},
				}},
			}},
			TlsParams: &tlsv3.TlsParameters{TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3, TlsMaximumProtocolVersion: tlsv3.TlsParameters_TLSv1_3},
		},
	})
	if err != nil {
		return nil, err
	}
	h2, err := anypb.New(&httpv3.HttpProtocolOptions{UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
		ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
			Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
		}},
	}})
	if err != nil {
		return nil, err
	}
	b := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: NodeID, Cluster: "devopus"},
		Admin: &bootstrapv3.Admin{
			// Inside the container only: `docker exec devopus-proxy curl localhost:9901/…`.
			Address: socketAddress("127.0.0.1", 9901),
		},
		DynamicResources: &bootstrapv3.Bootstrap_DynamicResources{
			AdsConfig: &corev3.ApiConfigSource{
				ApiType:                   corev3.ApiConfigSource_GRPC,
				TransportApiVersion:       corev3.ApiVersion_V3,
				SetNodeOnFirstMessageOnly: true,
				GrpcServices: []*corev3.GrpcService{{TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "xds"},
				}}},
			},
			LdsConfig: ads(),
			CdsConfig: ads(),
		},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Clusters: []*clusterv3.Cluster{{
			Name:                          "xds",
			ConnectTimeout:                durationpb.New(2 * time.Second),
			ClusterDiscoveryType:          &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS},
			DnsLookupFamily:               clusterv3.Cluster_V4_ONLY,
			TypedExtensionProtocolOptions: map[string]*anypb.Any{"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": h2},
			TransportSocket:               &corev3.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: upstreamTLS}},
			LoadAssignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: "xds",
				Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(HostGateway, uint32(xdsPort))}},
				}}}},
			},
		}}},
	}
	if err := b.ValidateAll(); err != nil {
		return nil, err
	}
	return protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(b)
}

// Hosts lists every host of the sites, for messages.
func (c Config) Hosts() []string {
	var out []string
	for _, s := range c.Sites {
		out = append(out, s.Hosts...)
	}
	sort.Strings(out)
	return out
}

// URL is how people open a host with the given ports.
func URL(host string, ports Ports) string {
	if ports.HTTPS == 443 {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%d", host, ports.HTTPS)
}

// IsPortConflict reports whether a start error means a host port is taken.
func IsPortConflict(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "address already in use") || strings.Contains(s, "port is already allocated") || strings.Contains(s, "ports are not available")
}

package proxy

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Headers between the proxy and the agent's waker (P-7).
const (
	// HeaderEnv and HeaderUnit tell the waker which environment and unit a request was
	// for; the proxy sets them on every request to a unit that sleeps.
	HeaderEnv  = "x-devopus-env"
	HeaderUnit = "x-devopus-unit"
	// HeaderDirect sends a request straight to the unit, past the waker: the waker
	// replays held requests with it once the unit is up.
	HeaderDirect = "x-devopus-direct"
)

// clusterWaker is the agent's waker on the host.
const clusterWaker = "devopus-waker"

// sleepyRoutes send a path prefix to the unit while it is awake and to the waker while
// it sleeps. The switch is in the route table, which every worker of Envoy picks up at
// once; endpoint health through an aggregate cluster left some workers sending to a
// stopped container for seconds. The waker's replays always go to the unit.
func sleepyRoutes(cluster, prefix, env, unit string, awake bool) []*routev3.Route {
	direct := &routev3.Route{
		Match: withHeader(prefixMatch(prefix), HeaderDirect),
		// No timeout: dev servers compile on the first request, streams stay open.
		Action:                 &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}, Timeout: durationpb.New(0)}},
		RequestHeadersToRemove: []string{HeaderDirect},
	}
	if awake {
		return []*routev3.Route{direct, {
			Match:  prefixMatch(prefix),
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cluster}, Timeout: durationpb.New(0)}},
		}}
	}
	return []*routev3.Route{direct, {
		Match:  prefixMatch(prefix),
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: clusterWaker}, Timeout: durationpb.New(0)}},
		RequestHeadersToAdd: []*corev3.HeaderValueOption{
			{Header: &corev3.HeaderValue{Key: HeaderEnv, Value: env}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			{Header: &corev3.HeaderValue{Key: HeaderUnit, Value: unit}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
		},
	}}
}

func withHeader(m *routev3.RouteMatch, header string) *routev3.RouteMatch {
	m.Headers = []*routev3.HeaderMatcher{{Name: header, HeaderMatchSpecifier: &routev3.HeaderMatcher_PresentMatch{PresentMatch: true}}}
	return m
}

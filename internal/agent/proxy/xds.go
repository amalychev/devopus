package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	metricsv3 "github.com/envoyproxy/go-control-plane/envoy/service/metrics/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/amalychev/devopus/internal/agent/pki"
)

// NodeID is the node Envoy announces in its bootstrap.
const NodeID = "devopus-proxy"

// XDS serves Envoy's configuration over ADS with mutual TLS. It listens on the host's
// loopback only; Envoy reaches it through host.docker.internal.
type XDS struct {
	cache cache.SnapshotCache
	grpc  *grpc.Server
	ln    net.Listener

	mu        sync.Mutex
	version   string
	streams   int
	connected time.Time
	nack      string
	// routesAcked is the last version of the route tables Envoy applied.
	routesAcked string
	applied     chan struct{}
	// requests receives the request counts of the clusters (see StatsInterval).
	requests func(cluster string, n uint64)
}

// StatsInterval is how often Envoy reports the request counters of its clusters.
const StatsInterval = 5 * time.Second

// OnRequests receives, every StatsInterval, how many requests each cluster got since the
// last report; clusters without requests are left out.
func (x *XDS) OnRequests(fn func(cluster string, n uint64)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.requests = fn
}

// WaitApplied waits until Envoy applied the route tables of version: after that, the
// traffic of a feature goes where the agent switched it (P-7: to the waker, then stop).
func (x *XDS) WaitApplied(ctx context.Context, version string) error {
	for {
		x.mu.Lock()
		done, ch := x.routesAcked == version || x.streams == 0, x.applied
		x.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// StreamMetrics receives Envoy's stats sink.
func (x *XDS) StreamMetrics(stream grpc.ClientStreamingServer[metricsv3.StreamMetricsMessage, metricsv3.StreamMetricsResponse]) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return stream.SendAndClose(&metricsv3.StreamMetricsResponse{})
			}
			return err
		}
		x.mu.Lock()
		fn := x.requests
		x.mu.Unlock()
		if fn == nil {
			continue
		}
		for _, mf := range msg.EnvoyMetrics {
			name := mf.GetName()
			cluster, ok := strings.CutPrefix(name, "cluster.")
			if !ok {
				continue
			}
			cluster, ok = strings.CutSuffix(cluster, ".upstream_rq_total")
			if !ok {
				continue
			}
			var n float64
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
			if n > 0 {
				fn(cluster, uint64(n))
			}
		}
	}
}

// ListenXDS opens the xDS port. addr is usually 127.0.0.1:<port>; port 0 picks one.
func ListenXDS(addr string, ctl *pki.Control) (*XDS, error) {
	tlsCfg, err := ctl.ServerTLS()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	x := &XDS{cache: cache.NewSnapshotCache(true, cache.IDHash{}, nil), ln: ln, applied: make(chan struct{})}
	x.grpc = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		// Envoy keeps one long ADS stream; keepalive notices a vanished container.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	srv := server.NewServer(context.Background(), x.cache, server.CallbackFuncs{
		StreamOpenFunc: func(context.Context, int64, string) error {
			x.mu.Lock()
			x.streams++
			x.connected = time.Now()
			x.mu.Unlock()
			return nil
		},
		StreamClosedFunc: func(int64, *corev3.Node) {
			x.mu.Lock()
			x.streams--
			x.mu.Unlock()
		},
		StreamRequestFunc: func(_ int64, req *discoveryv3.DiscoveryRequest) error {
			x.mu.Lock()
			defer x.mu.Unlock()
			if req.ErrorDetail != nil {
				x.nack = req.ErrorDetail.Message
			} else if req.VersionInfo == x.version {
				x.nack = ""
			}
			if req.ErrorDetail == nil && req.TypeUrl == resource.RouteType && req.VersionInfo != x.routesAcked {
				x.routesAcked = req.VersionInfo
				close(x.applied)
				x.applied = make(chan struct{})
			}
			return nil
		},
	})
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(x.grpc, srv)
	metricsv3.RegisterMetricsServiceServer(x.grpc, x)
	return x, nil
}

// Port is the TCP port of the xDS server.
func (x *XDS) Port() int { return x.ln.Addr().(*net.TCPAddr).Port }

// Serve blocks until Close.
func (x *XDS) Serve() error {
	err := x.grpc.Serve(x.ln)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

func (x *XDS) Close() { x.grpc.Stop() }

// Set makes cfg Envoy's configuration and returns its version. Setting the same
// configuration again is free: the version does not change and Envoy is not told.
func (x *XDS) Set(ctx context.Context, cfg Config) (string, error) {
	res, version, err := cfg.Resources()
	if err != nil {
		return "", err
	}
	x.mu.Lock()
	same := version == x.version
	x.mu.Unlock()
	if same {
		return version, nil
	}
	snap, err := cache.NewSnapshot(version, res)
	if err != nil {
		return "", err
	}
	if err := snap.Consistent(); err != nil {
		return "", err
	}
	if err := x.cache.SetSnapshot(ctx, NodeID, snap); err != nil {
		return "", err
	}
	x.mu.Lock()
	x.version = version
	x.mu.Unlock()
	return version, nil
}

// XDSState is what the agent knows about its link to Envoy.
type XDSState struct {
	Connected bool
	Version   string
	// Rejected is Envoy's last NACK for the current version.
	Rejected string
}

func (x *XDS) State() XDSState {
	x.mu.Lock()
	defer x.mu.Unlock()
	return XDSState{Connected: x.streams > 0, Version: x.version, Rejected: x.nack}
}

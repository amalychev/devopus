// Package runtime talks to the container runtime through the Docker Engine API.
// Docker Desktop, OrbStack, Colima and Podman all serve it; they differ in where the
// socket is and in details the agent works around elsewhere.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Labels on everything the agent creates. GC only ever touches resources with Managed.
const (
	LabelManaged = "com.devopus.managed"
	LabelProject = "com.devopus.project"
	LabelEnv     = "com.devopus.env"
	LabelKind    = "com.devopus.kind"
)

// Endpoint is where the Docker API is and which product serves it.
type Endpoint struct {
	Host    string // unix:///…/docker.sock, tcp://…
	Context string // docker context it came from, if any
	Name    string // docker-desktop, orbstack, colima, podman, docker-engine, unknown
}

// Detect finds the endpoint the docker CLI would use: $DOCKER_HOST, then the
// $DOCKER_CONTEXT or current context of ~/.docker/config.json, then the default socket.
func Detect() Endpoint {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return Endpoint{Host: h, Name: nameFromHost(h)}
	}
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".docker")
	}
	ctxName := os.Getenv("DOCKER_CONTEXT")
	if ctxName == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
		ctxName = cfg.CurrentContext
	}
	if ctxName != "" && ctxName != "default" {
		sum := sha256.Sum256([]byte(ctxName))
		var meta struct {
			Endpoints map[string]struct{ Host string } `json:"Endpoints"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")); err == nil {
			if json.Unmarshal(data, &meta) == nil && meta.Endpoints["docker"].Host != "" {
				h := meta.Endpoints["docker"].Host
				return Endpoint{Host: h, Context: ctxName, Name: nameFromHost(h)}
			}
		}
	}
	h := "unix:///var/run/docker.sock"
	return Endpoint{Host: h, Name: nameFromHost(h)}
}

func nameFromHost(h string) string {
	switch {
	case strings.Contains(h, ".orbstack"):
		return "orbstack"
	case strings.Contains(h, ".colima"):
		return "colima"
	case strings.Contains(h, "podman"):
		return "podman"
	case strings.Contains(h, "/.docker/run/") || strings.Contains(h, "docker_engine"):
		return "docker-desktop"
	}
	return "unknown"
}

// Network is a docker network as the agent sees it.
type Network struct {
	ID      string
	Name    string
	Subnets []netip.Prefix
	Labels  map[string]string
}

// Info describes the runtime.
type Info struct {
	Name          string
	ServerVersion string
	OS            string
}

// Docker is the runtime behind the Docker Engine API.
type Docker struct {
	cli      *client.Client
	endpoint Endpoint
}

func New(ep Endpoint) (*Docker, error) {
	cli, err := client.New(client.WithHost(ep.Host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli, endpoint: ep}, nil
}

func (d *Docker) Endpoint() Endpoint { return d.endpoint }

func (d *Docker) Close() error { return d.cli.Close() }

func (d *Docker) Info(ctx context.Context) (Info, error) {
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Info{}, err
	}
	name := d.endpoint.Name
	osName := res.Info.OperatingSystem
	switch {
	case strings.Contains(osName, "Docker Desktop"):
		name = "docker-desktop"
	case strings.Contains(strings.ToLower(osName), "orbstack"):
		name = "orbstack"
	case name == "unknown" && res.Info.OSType == "linux":
		name = "docker-engine"
	}
	return Info{Name: name, ServerVersion: res.Info.ServerVersion, OS: osName}, nil
}

// Networks lists every network; the agent needs foreign ones too, to avoid their subnets.
func (d *Docker) Networks(ctx context.Context) ([]Network, error) {
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		nw := Network{ID: n.ID, Name: n.Name, Labels: n.Labels}
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() {
				nw.Subnets = append(nw.Subnets, c.Subnet)
			}
		}
		out = append(out, nw)
	}
	return out, nil
}

// ErrSubnetTaken means the runtime refused the subnet: something else uses it.
var ErrSubnetTaken = errors.New("subnet overlaps an existing network")

// CreateNetwork creates a bridge network. Docker hands out addresses only from the
// lower half of the subnet; the upper half is for the fixed addresses the agent
// assigns (the proxy, the DNS server, applications that must keep their IP across restarts).
func (d *Docker) CreateNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) (string, error) {
	ipam := network.IPAMConfig{Subnet: subnet}
	if subnet.Addr().Is4() && subnet.Bits() < 31 {
		ipam.IPRange = netip.PrefixFrom(subnet.Masked().Addr(), subnet.Bits()+1)
	}
	res, err := d.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		IPAM:   &network.IPAM{Config: []network.IPAMConfig{ipam}},
		Labels: labels,
	})
	if err != nil {
		if strings.Contains(err.Error(), "overlap") {
			return "", fmt.Errorf("%w: %v", ErrSubnetTaken, err)
		}
		return "", err
	}
	return res.ID, nil
}

// RemoveNetwork removes a network; one that is already gone is not an error.
func (d *Docker) RemoveNetwork(ctx context.Context, id string) error {
	_, err := d.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

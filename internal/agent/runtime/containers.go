package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// More labels, on containers.
const (
	// LabelInstance separates agents with different data directories (a test agent
	// next to the real one): each observes and collects only its own resources.
	LabelInstance = "com.docklet.instance"
	// LabelRole is proxy, dns, module, service…
	LabelRole = "com.docklet.role"
	// LabelConfig is a hash of what the container was created from; a different hash
	// means the container must be recreated.
	LabelConfig = "com.docklet.config"
)

// Container is a container as the agent sees it.
type Container struct {
	ID      string
	Name    string
	Image   string
	Labels  map[string]string
	Running bool
	State   string
	// Networks maps network IDs to the container's address in them.
	Networks map[string]netip.Addr
}

// ContainerSpec is what the agent creates.
type ContainerSpec struct {
	Name       string
	Image      string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	Ports      []PortBinding
	ExtraHosts []string
	// Network and IP attach the container to its first network at a fixed address.
	Network string
	IP      netip.Addr
	// DNS servers the container's embedded resolver forwards to.
	DNS     []netip.Addr
	Files   []File
	Restart string // "", unless-stopped…
	// User runs the process as this uid; empty keeps the image's default.
	User string
}

// PortBinding publishes ContainerPort on HostIP:HostPort.
type PortBinding struct {
	HostIP        netip.Addr
	HostPort      uint16
	ContainerPort uint16
}

// File is copied into the container before it starts.
type File struct {
	Path string
	Data []byte
	Mode int64
	UID  int
	GID  int
}

// Containers lists every container of this agent instance, running or not.
func (d *Docker) Containers(ctx context.Context, instance string) ([]Container, error) {
	f := client.Filters{}.Add("label", LabelManaged+"=true")
	if instance != "" {
		f = f.Add("label", LabelInstance+"="+instance)
	}
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, c := range res.Items {
		ct := Container{ID: c.ID, Image: c.Image, Labels: c.Labels, State: string(c.State), Running: c.State == container.StateRunning, Networks: map[string]netip.Addr{}}
		if len(c.Names) > 0 {
			ct.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		if c.NetworkSettings != nil {
			for _, ep := range c.NetworkSettings.Networks {
				if ep != nil && ep.NetworkID != "" {
					ct.Networks[ep.NetworkID] = ep.IPAddress
				}
			}
		}
		out = append(out, ct)
	}
	return out, nil
}

// EnsureImage pulls ref unless it is already present.
func (d *Docker) EnsureImage(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	res, err := d.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer res.Close()
	if err := res.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// CreateContainer creates a container and copies its files in; it does not start it.
func (d *Docker) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	cfg := &container.Config{Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, Labels: spec.Labels, User: spec.User}
	host := &container.HostConfig{ExtraHosts: spec.ExtraHosts, DNS: spec.DNS}
	if spec.Restart != "" {
		host.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyMode(spec.Restart)}
	}
	if len(spec.Ports) > 0 {
		cfg.ExposedPorts = network.PortSet{}
		host.PortBindings = network.PortMap{}
		for _, p := range spec.Ports {
			port, err := network.ParsePort(strconv.Itoa(int(p.ContainerPort)) + "/tcp")
			if err != nil {
				return "", err
			}
			cfg.ExposedPorts[port] = struct{}{}
			host.PortBindings[port] = append(host.PortBindings[port], network.PortBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(int(p.HostPort))})
		}
	}
	var netCfg *network.NetworkingConfig
	if spec.Network != "" {
		host.NetworkMode = container.NetworkMode(spec.Network)
		ep := &network.EndpointSettings{}
		if spec.IP.IsValid() {
			ep.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: spec.IP}
		}
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{spec.Network: ep}}
	}
	res, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Name: spec.Name, Config: cfg, HostConfig: host, NetworkingConfig: netCfg})
	if err != nil {
		return "", err
	}
	if len(spec.Files) > 0 {
		if err := d.copyFiles(ctx, res.ID, spec.Files); err != nil {
			_ = d.RemoveContainer(ctx, res.ID)
			return "", err
		}
	}
	return res.ID, nil
}

// copyFiles writes files into the container through a tar stream: no bind mounts, so
// it works the same whatever the runtime shares with its VM.
func (d *Docker) copyFiles(ctx context.Context, id string, files []File) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	now := time.Now()
	for _, f := range files {
		// Parent directories first, owned like the file so the process can read it.
		var parents []string
		for dir := path.Dir(f.Path); dir != "/" && dir != "." && !dirs[dir]; dir = path.Dir(dir) {
			parents = append([]string{dir}, parents...)
			dirs[dir] = true
		}
		for _, dir := range parents {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(dir, "/") + "/", Mode: 0o755, Uid: f.UID, Gid: f.GID, ModTime: now}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(f.Path, "/"), Mode: f.Mode, Size: int64(len(f.Data)), Uid: f.UID, Gid: f.GID, ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	_, err := d.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf, CopyUIDGID: true})
	return err
}

func (d *Docker) StartContainer(ctx context.Context, id string) error {
	_, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// RemoveContainer stops and removes a container; one that is already gone is not an error.
func (d *Docker) RemoveContainer(ctx context.Context, id string) error {
	_, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// ConnectNetwork attaches a running container to a network at a fixed address.
func (d *Docker) ConnectNetwork(ctx context.Context, networkID, containerID string, ip netip.Addr) error {
	ep := &network.EndpointSettings{}
	if ip.IsValid() {
		ep.IPAMConfig = &network.EndpointIPAMConfig{IPv4Address: ip}
	}
	_, err := d.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: containerID, EndpointConfig: ep})
	return err
}

// DisconnectNetwork detaches a container; a container or network that is gone is not an error.
func (d *Docker) DisconnectNetwork(ctx context.Context, networkID, containerID string) error {
	_, err := d.cli.NetworkDisconnect(ctx, networkID, client.NetworkDisconnectOptions{Container: containerID, Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerLogs returns the last lines of a container's output, for error messages.
func (d *Docker) ContainerLogs(ctx context.Context, id string, lines int) string {
	res, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(lines)})
	if err != nil {
		return ""
	}
	defer res.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, res)
	return strings.TrimSpace(out.String())
}

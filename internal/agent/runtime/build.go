package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// BuildSpec is an image build of an application.
type BuildSpec struct {
	Tag        string
	Context    string // absolute folder on the host
	Dockerfile string // relative to Context; empty: Dockerfile
	Target     string
	Args       map[string]string
	Labels     map[string]string
}

// ImageExists reports whether the runtime has the image.
func (d *Docker) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, ref)
	switch {
	case err == nil:
		return true, nil
	case errdefs.IsNotFound(err):
		return false, nil
	}
	return false, err
}

// Build builds an image with the docker CLI against the agent's endpoint. The CLI
// brings BuildKit, .dockerignore, cache mounts and SSH forwarding, which the Engine
// API alone does not. The whole output goes to log; the error carries its tail.
func (d *Docker) Build(ctx context.Context, spec BuildSpec, log io.Writer) error {
	args := []string{"build", "--tag", spec.Tag}
	if spec.Dockerfile != "" {
		args = append(args, "--file", spec.Dockerfile)
	}
	if spec.Target != "" {
		args = append(args, "--target", spec.Target)
	}
	keys := make([]string, 0, len(spec.Args))
	for k := range spec.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+spec.Args[k])
	}
	for k, v := range spec.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = spec.Context
	cmd.Env = append(withoutDockerEnv(os.Environ()), "DOCKER_HOST="+d.endpoint.Host, "BUILDKIT_PROGRESS=plain")
	var tail tailBuffer
	cmd.Stdout = io.MultiWriter(log, &tail)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		if out := strings.TrimSpace(tail.String()); out != "" {
			return fmt.Errorf("docker build: %w\n%s", err, out)
		}
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

// DockerEnv is the environment for running the docker CLI against this runtime.
func (d *Docker) DockerEnv() []string {
	return append(withoutDockerEnv(os.Environ()), "DOCKER_HOST="+d.endpoint.Host)
}

func withoutDockerEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			out = append(out, kv)
		}
	}
	return out
}

// tailBuffer keeps the last lines written to it.
type tailBuffer struct{ buf bytes.Buffer }

const tailLines = 15

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 64<<10 {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte{}, b[len(b)-32<<10:]...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return LastLines(t.buf.String(), tailLines) }

// LastLines returns the last n lines of s.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Image is an image the agent built.
type Image struct {
	ID     string
	Tags   []string
	Labels map[string]string
}

// Images lists the images this agent instance built.
func (d *Docker) Images(ctx context.Context, instance string) ([]Image, error) {
	f := client.Filters{}.Add("label", LabelManaged+"=true").Add("label", LabelInstance+"="+instance)
	res, err := d.cli.ImageList(ctx, client.ImageListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(res.Items))
	for _, img := range res.Items {
		out = append(out, Image{ID: img.ID, Tags: img.RepoTags, Labels: img.Labels})
	}
	return out, nil
}

// RemoveImage removes an image; one that is gone is not an error, one in use is.
func (d *Docker) RemoveImage(ctx context.Context, id string) error {
	_, err := d.cli.ImageRemove(ctx, id, client.ImageRemoveOptions{PruneChildren: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

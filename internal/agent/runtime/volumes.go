package runtime

import (
	"context"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Volume is a named volume as the agent sees it.
type Volume struct {
	Name   string
	Labels map[string]string
}

// Volumes lists the volumes of this agent instance.
func (d *Docker) Volumes(ctx context.Context, instance string) ([]Volume, error) {
	f := client.Filters{}.Add("label", LabelManaged+"=true")
	if instance != "" {
		f = f.Add("label", LabelInstance+"="+instance)
	}
	res, err := d.cli.VolumeList(ctx, client.VolumeListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, Volume{Name: v.Name, Labels: v.Labels})
	}
	return out, nil
}

// CreateVolume creates a labelled volume; one that exists is left as it is.
func (d *Docker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
	return err
}

// RemoveVolume removes a volume with its data; one that is already gone is not an error.
func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	_, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

package compose

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/devcorb/devcorb/internal/workspace"
)

// Pinner resolves an image tag to the digest it points to today.
type Pinner interface {
	Digest(ctx context.Context, image string) (string, error)
}

// ErrRateLimited means the registry refused the lookup for now (Docker Hub answers 429
// to anonymous clients); the image should be pinned on a later attempt.
var ErrRateLimited = errors.New("registry rate limit")

// RegistryPinner asks the registry, with the credentials of the local Docker config.
type RegistryPinner struct{}

func (RegistryPinner) Digest(ctx context.Context, image string) (string, error) {
	d, err := crane.Digest(image, crane.WithContext(ctx), crane.WithAuthFromKeychain(authn.DefaultKeychain))
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	return d, err
}

// pin returns the image pinned to a digest when it floats and the pinner can resolve it.
func pin(ctx context.Context, rep *Report, p Pinner, name, image string) string {
	if workspace.ImagePinned(image) {
		return image
	}
	if p == nil {
		rep.Unpinned = append(rep.Unpinned, name+": "+image)
		return image
	}
	digest, err := p.Digest(ctx, image)
	switch {
	case err == nil:
		pinned := strings.SplitN(image, "@", 2)[0] + "@" + digest
		rep.Pinned = append(rep.Pinned, name+": "+image+" → "+shortDigest(digest))
		return pinned
	case errors.Is(err, ErrRateLimited):
		rep.PinLater = append(rep.PinLater, name+": "+image)
	default:
		rep.Unpinned = append(rep.Unpinned, name+": "+image)
	}
	return image
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "…"
	}
	return d
}

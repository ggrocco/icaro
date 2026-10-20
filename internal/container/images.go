package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

// PinStore persists image pins (store.Store implements it).
type PinStore interface {
	PinImage(ctx context.Context, ref, digest string) error
	GetPinnedImage(ctx context.Context, ref string) (*store.PinnedImage, error)
	ListPinnedImages(ctx context.Context) ([]*store.PinnedImage, error)
}

// Images resolves image references to pinned digests: the first use of a
// ref pins the digest present locally (pulling if needed), and every later
// container is created from that exact digest.
type Images struct {
	Engine Engine
	Pins   PinStore
}

// Resolve returns the digest reference to create containers from.
func (i *Images) Resolve(ctx context.Context, ref string) (string, error) {
	p, err := i.Pins.GetPinnedImage(ctx, ref)
	switch {
	case err == nil:
		if _, err := i.Engine.ImageDigest(ctx, p.Digest); err == nil {
			return p.Digest, nil
		}
		if !isRepoDigest(p.Digest) {
			return "", fmt.Errorf("pinned image %s (%s) is gone and was built locally; re-run `icaro setup`", ref, p.Digest)
		}
		if err := i.Engine.PullImage(ctx, p.Digest); err != nil {
			return "", err
		}
		return p.Digest, nil
	case errors.Is(err, store.ErrNotFound):
		return i.Pin(ctx, ref, false)
	default:
		return "", err
	}
}

// Pin records the digest of ref, pulling first when pull is set or the
// image is missing.
func (i *Images) Pin(ctx context.Context, ref string, pull bool) (string, error) {
	if pull {
		if err := i.Engine.PullImage(ctx, ref); err != nil {
			return "", err
		}
	} else if err := i.Engine.EnsureImage(ctx, ref); err != nil {
		return "", err
	}
	d, err := i.Engine.ImageDigest(ctx, ref)
	if err != nil {
		return "", err
	}
	if err := i.Pins.PinImage(ctx, ref, d); err != nil {
		return "", err
	}
	return d, nil
}

// Problem is one pinned image that no longer verifies.
type Problem struct {
	Ref, Digest, Reason string
	Fixable             bool
}

// Verify checks that every pinned digest is still present locally.
func (i *Images) Verify(ctx context.Context) ([]Problem, error) {
	pins, err := i.Pins.ListPinnedImages(ctx)
	if err != nil {
		return nil, err
	}
	var out []Problem
	for _, p := range pins {
		if _, err := i.Engine.ImageDigest(ctx, p.Digest); err != nil {
			reason := "pinned digest is missing locally"
			if !errors.Is(err, docker.ErrNotFound) {
				reason = err.Error()
			}
			out = append(out, Problem{Ref: p.Ref, Digest: p.Digest, Reason: reason, Fixable: isRepoDigest(p.Digest)})
		}
	}
	return out, nil
}

// Repair re-pulls a problem image by its known digest.
func (i *Images) Repair(ctx context.Context, p Problem) error {
	if !p.Fixable {
		return fmt.Errorf("%s was built locally; rebuild it and re-run `icaro setup`", p.Ref)
	}
	return i.Engine.PullImage(ctx, p.Digest)
}

func isRepoDigest(d string) bool { return strings.Contains(d, "@sha256:") }

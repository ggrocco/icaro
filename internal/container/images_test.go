package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icaro/internal/store"
)

// failPins is memPins with injectable errors.
type failPins struct {
	memPins
	get, list, pin error
}

func (p *failPins) GetPinnedImage(ctx context.Context, ref string) (*store.PinnedImage, error) {
	if p.get != nil {
		return nil, p.get
	}
	return p.memPins.GetPinnedImage(ctx, ref)
}
func (p *failPins) ListPinnedImages(ctx context.Context) ([]*store.PinnedImage, error) {
	if p.list != nil {
		return nil, p.list
	}
	return p.memPins.ListPinnedImages(ctx)
}
func (p *failPins) PinImage(ctx context.Context, ref, d string) error {
	if p.pin != nil {
		return p.pin
	}
	return p.memPins.PinImage(ctx, ref, d)
}

func TestImagesResolveRepullsGoneRepoDigest(t *testing.T) {
	f := newFailEngine(nil)
	pins := &failPins{memPins: memPins{m: map[string]string{"agent:1": "agent@sha256:2"}}}
	im := &Images{Engine: f, Pins: pins}
	ctx := context.Background()
	// The pinned digest is gone locally but came from a registry: it is
	// pulled back by digest, never re-resolved from the tag.
	d, err := im.Resolve(ctx, "agent:1")
	if err != nil || d != "agent@sha256:2" || len(f.pulled) != 1 || f.pulled[0] != "agent@sha256:2" {
		t.Fatalf("%s %v %v", d, err, f.pulled)
	}
	delete(f.images, "agent@sha256:2")
	f.fail = map[string]error{"PullImage": errors.New("offline")}
	if _, err := im.Resolve(ctx, "agent:1"); err == nil || err.Error() != "offline" {
		t.Fatal(err)
	}
}

func TestImagesErrors(t *testing.T) {
	boom := errors.New("boom")
	ctx := context.Background()
	newIm := func(fail map[string]error) (*Images, *failPins) {
		pins := &failPins{memPins: memPins{m: map[string]string{}}}
		return &Images{Engine: newFailEngine(fail), Pins: pins}, pins
	}

	im, pins := newIm(nil)
	pins.get = boom
	if _, err := im.Resolve(ctx, "a:1"); !errors.Is(err, boom) {
		t.Fatalf("store error must surface, not pin: %v", err)
	}

	for _, c := range []struct {
		method string
		pull   bool
	}{{"PullImage", true}, {"EnsureImage", false}, {"ImageDigest", false}} {
		im, pins := newIm(map[string]error{c.method: boom})
		if _, err := im.Pin(ctx, "a:1", c.pull); !errors.Is(err, boom) || len(pins.m) != 0 {
			t.Fatalf("%s: %v %v", c.method, err, pins.m)
		}
	}
	im, pins = newIm(nil)
	pins.pin = boom
	if _, err := im.Pin(ctx, "a:1", false); !errors.Is(err, boom) {
		t.Fatal(err)
	}

	im, pins = newIm(nil)
	pins.list = boom
	if _, err := im.Verify(ctx); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// An engine error other than "not found" is reported as the reason.
	im, pins = newIm(map[string]error{"ImageDigest": errors.New("daemon unreachable")})
	pins.m["a:1"] = "a@sha256:1"
	probs, err := im.Verify(ctx)
	if err != nil || len(probs) != 1 || probs[0].Reason != "daemon unreachable" || !probs[0].Fixable {
		t.Fatalf("%+v %v", probs, err)
	}
	if err := (&Images{}).Repair(ctx, Problem{Ref: "local:1"}); err == nil || !strings.Contains(err.Error(), "built locally") {
		t.Fatal(err)
	}
}

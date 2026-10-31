package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// muxFrame encodes one docker log frame (stdcopy format): a 8-byte header
// with the stream type and big-endian payload length, then the payload.
func muxFrame(stream byte, payload []byte) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload))) //nolint:gosec // test payloads are small
	return append(hdr, payload...)
}

// Behavior is what a fake container does when started.
type Behavior struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Output   []byte        // content of /icaro/output.json; nil => file absent
	Duration time.Duration // how long the container "runs" before exiting
}

// FakeContainer is the recorded state of a fake container.
type FakeContainer struct {
	ID       string
	Spec     Spec
	Files    map[string][]byte // files copied in, keyed by path
	Started  bool
	Killed   bool
	Removed  bool
	Behavior Behavior
	done     chan struct{}
	exit     int
}

// Fake is an in-memory Client for tests. OnStart decides a container's
// behavior from its spec and copied-in files.
type Fake struct {
	// turn is a one-slot token: holding it is exclusive access to the
	// fake's state, so the goroutines of a test take turns.
	turn       chan struct{}
	seq        int
	Containers map[string]*FakeContainer
	Volumes    map[string]bool
	Images     map[string]bool
	PullErr    error
	OnStart    func(c *FakeContainer) Behavior
}

// NewFake returns an empty fake engine.
func NewFake() *Fake {
	return &Fake{turn: make(chan struct{}, 1), Containers: map[string]*FakeContainer{}, Volumes: map[string]bool{}, Images: map[string]bool{}}
}

func (f *Fake) acquire() { f.turn <- struct{}{} }
func (f *Fake) release() { <-f.turn }

func (f *Fake) Ping(context.Context) error { return nil }

func (f *Fake) EnsureImage(_ context.Context, ref string) error {
	f.acquire()
	defer f.release()
	if f.PullErr != nil && !f.Images[ref] {
		return f.PullErr
	}
	f.Images[ref] = true
	return nil
}

func (f *Fake) CreateVolume(_ context.Context, name string, _ map[string]string) error {
	f.acquire()
	defer f.release()
	f.Volumes[name] = true
	return nil
}

func (f *Fake) RemoveVolume(_ context.Context, name string) error {
	f.acquire()
	defer f.release()
	delete(f.Volumes, name)
	return nil
}

func (f *Fake) CreateContainer(_ context.Context, spec Spec) (string, error) {
	f.acquire()
	defer f.release()
	f.seq++
	id := fmt.Sprintf("ctr%03d", f.seq)
	f.Containers[id] = &FakeContainer{ID: id, Spec: spec, Files: map[string][]byte{}, done: make(chan struct{})}
	return id, nil
}

// AddContainer registers a pre-existing container (reconcile tests).
func (f *Fake) AddContainer(id string, spec Spec, b Behavior, started bool) *FakeContainer {
	f.acquire()
	defer f.release()
	c := &FakeContainer{ID: id, Spec: spec, Files: map[string][]byte{}, Behavior: b, done: make(chan struct{})}
	f.Containers[id] = c
	if started {
		c.Started = true
		go f.finish(c)
	}
	return c
}

func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.acquire()
	c, ok := f.Containers[id]
	if !ok {
		f.release()
		return ErrNotFound
	}
	if f.OnStart != nil {
		c.Behavior = f.OnStart(c)
	}
	c.Started = true
	f.release()
	go f.finish(c)
	return nil
}

func (f *Fake) finish(c *FakeContainer) {
	if c.Behavior.Duration > 0 {
		t := time.NewTimer(c.Behavior.Duration)
		select {
		case <-t.C:
		case <-c.done:
			return
		}
	}
	f.acquire()
	defer f.release()
	select {
	case <-c.done:
	default:
		c.exit = c.Behavior.ExitCode
		close(c.done)
	}
}

func (f *Fake) CopyIn(_ context.Context, id, destDir string, r io.Reader) error {
	f.acquire()
	c, ok := f.Containers[id]
	f.release()
	if !ok {
		return ErrNotFound
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		f.acquire()
		c.Files[strings.TrimSuffix(destDir, "/")+"/"+h.Name] = b
		f.release()
	}
}

func (f *Fake) CopyOut(_ context.Context, id, path string) (io.ReadCloser, error) {
	f.acquire()
	c, ok := f.Containers[id]
	f.release()
	if !ok {
		return nil, ErrNotFound
	}
	var content []byte
	if c.Behavior.Output != nil && strings.HasSuffix(path, "output.json") {
		content = c.Behavior.Output
	} else if b, ok := c.Files[path]; ok {
		content = b
	} else {
		return nil, ErrNotFound
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	name := path[strings.LastIndex(path, "/")+1:]
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()
	return io.NopCloser(&buf), nil
}

func (f *Fake) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	f.acquire()
	c, ok := f.Containers[id]
	f.release()
	if !ok {
		return nil, ErrNotFound
	}
	pr, pw := io.Pipe()
	go func() {
		if follow {
			// Like the real client, a cancelled context ends a follow.
			select {
			case <-c.done:
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				return
			}
		}
		f.acquire()
		out, errOut := c.Behavior.Stdout, c.Behavior.Stderr
		f.release()
		if out != "" {
			_, _ = pw.Write(muxFrame(1, []byte(out)))
		}
		if errOut != "" {
			_, _ = pw.Write(muxFrame(2, []byte(errOut)))
		}
		_ = pw.Close()
	}()
	return pr, nil
}

func (f *Fake) Wait(ctx context.Context, id string) (int, error) {
	f.acquire()
	c, ok := f.Containers[id]
	f.release()
	if !ok {
		return -1, ErrNotFound
	}
	select {
	case <-c.done:
		f.acquire()
		defer f.release()
		return c.exit, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (f *Fake) Inspect(_ context.Context, id string) (State, error) {
	f.acquire()
	defer f.release()
	c, ok := f.Containers[id]
	if !ok || c.Removed {
		return State{}, nil
	}
	st := State{Exists: true, Labels: c.Spec.Labels}
	select {
	case <-c.done:
		st.ExitCode = c.exit
	default:
		st.Running = c.Started
	}
	return st, nil
}

func (f *Fake) Kill(_ context.Context, id string) error {
	f.acquire()
	defer f.release()
	c, ok := f.Containers[id]
	if !ok {
		return nil
	}
	select {
	case <-c.done:
	default:
		c.Killed = true
		c.exit = 137
		close(c.done)
	}
	return nil
}

func (f *Fake) RemoveContainer(_ context.Context, id string) error {
	f.acquire()
	defer f.release()
	if c, ok := f.Containers[id]; ok {
		c.Removed = true
	}
	return nil
}

func (f *Fake) ListByLabel(_ context.Context, labels map[string]string) ([]string, error) {
	f.acquire()
	defer f.release()
	var ids []string
	for id, c := range f.Containers {
		if c.Removed {
			continue
		}
		match := true
		for k, v := range labels {
			if c.Spec.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// IsStarted reports whether a container has been started (lock-safe for tests).
func (f *Fake) IsStarted(id string) bool {
	f.acquire()
	defer f.release()
	c, ok := f.Containers[id]
	return ok && c.Started
}

// Container returns a snapshot of a fake container.
func (f *Fake) Container(id string) *FakeContainer {
	f.acquire()
	defer f.release()
	return f.Containers[id]
}

var _ Client = (*Fake)(nil)

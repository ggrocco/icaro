//go:build integration

package container

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDistillContainerHasNoRouteOut: the memory distiller's container
// (ADR 0036) has no network at all, so a request to an outside host
// fails, and its models are read-only. The image is swapped for busybox
// with a shell; the rest of the spec is the one the daemon starts.
func TestDistillContainerHasNoRouteOut(t *testing.T) {
	eng := engine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	models, sock := t.TempDir(), t.TempDir()
	_ = os.Chmod(sock, 0o777)
	spec := DistillSpec{Name: "icaro-it-distill-" + time.Now().Format("150405"), Image: testImage, ModelsDir: models, SocketDir: sock,
		ModelFile: "m.gguf", Threads: 1, NanoCPUs: 1e9, MemoryBytes: 256 << 20}.Build()
	spec.Entrypoint = []string{"sh", "-c", `wget -T 3 -q -O- http://1.1.1.1/ >/dev/null 2>&1; echo "outside=$?";
		wget -T 3 -q -O- http://example.com/ >/dev/null 2>&1; echo "dns=$?";
		touch /models/x 2>/dev/null; echo "models=$?"; touch /sock/ok; echo "sock=$?"; cat /proc/net/route | wc -l`}
	spec.Cmd = nil
	id, err := eng.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.RemoveContainer(context.Background(), id) })
	if err := eng.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if code, err := eng.Wait(ctx, id); err != nil || code != 0 {
		t.Fatalf("exit %d %v", code, err)
	}
	rc, err := eng.Logs(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	out := string(b)
	for _, want := range []string{"outside=1", "dns=1", "models=1", "sock=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in:\n%s", want, out)
		}
	}
	// Only the header of the route table: no interface but loopback.
	if !strings.Contains(out, "\n1\n") && !strings.HasSuffix(strings.TrimSpace(out), "1") {
		t.Errorf("routes:\n%s", out)
	}
}

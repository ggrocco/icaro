package container

import (
	"slices"
	"strings"
	"testing"
)

func TestDistillSpec(t *testing.T) {
	s := DistillSpec{Name: "icaro-distill-x", User: "1000:1000", ModelsDir: "/m", SocketDir: "/s", ModelFile: "q.gguf",
		Threads: 2, NanoCPUs: 2e9, MemoryBytes: DistillMemory(1 << 30), Labels: map[string]string{LabelDistill: "x"}}
	spec := s.Build()
	hc := spec.HostConfig
	if spec.Image != DistillImage || !strings.Contains(spec.Image, "@sha256:") || spec.User != "1000:1000" {
		t.Fatalf("%+v", spec)
	}
	// No network at all, and the baseline hardening.
	if string(hc.NetworkMode) != "none" || !hc.ReadonlyRootfs || !slices.Contains(hc.CapDrop, "ALL") || hc.NanoCPUs != 2e9 || hc.Memory != 3<<30 {
		t.Fatalf("host config %+v", hc)
	}
	var models, sock bool
	for _, m := range hc.Mounts {
		switch m.Target {
		case "/models":
			models = m.Source == "/m" && m.ReadOnly
		case "/sock":
			sock = m.Source == "/s" && !m.ReadOnly
		default:
			t.Errorf("unexpected mount %+v", m)
		}
	}
	if !models || !sock {
		t.Fatalf("mounts %+v", hc.Mounts)
	}
	cmd := strings.Join(spec.Cmd, " ")
	if cmd != "-m /models/q.gguf --host /sock/llama.sock -c 12288 -t 2 -np 1 --no-webui --log-disable" {
		t.Fatal(cmd)
	}
	if s.String() == "" {
		t.Fatal("String")
	}
	// Defaults: one thread at least, the pinned image, an 8k context.
	d := DistillSpec{Image: "x", ModelFile: "m", Context: 4096}.Build()
	if d.Image != "x" || !strings.Contains(strings.Join(d.Cmd, " "), "-c 4096 -t 1 ") {
		t.Fatal(d.Cmd)
	}
}

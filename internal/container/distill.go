package container

import (
	"fmt"
	"strconv"

	"github.com/moby/moby/api/types/mount"

	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
)

// DistillImage is llama.cpp's server, pinned by digest, that runs the
// memory distiller's local model (ADR 0036).
const DistillImage = "ghcr.io/ggml-org/llama.cpp:server@sha256:4d2de15a0ffbce704777c64026b40512a71e0bbc0b303a5fe5ea9bf71760dff9"

// Paths inside the distiller container.
const (
	distillModels = "/models"
	distillSock   = "/sock"
	// DistillSocketName is the socket the server listens on, in SocketDir.
	DistillSocketName = "llama.sock"
)

// LabelDistill marks the distiller container of a state root.
const LabelDistill = "icaro.distill"

// DistillSpec is the memory distiller's model server: the helper baseline
// with no network at all. It listens on a Unix socket in SocketDir, which
// only the daemon also mounts, and reads ModelsDir read-only.
type DistillSpec struct {
	Name      string
	Image     string // default DistillImage
	User      string // uid:gid that owns SocketDir
	ModelsDir string
	SocketDir string
	ModelFile string
	// Threads and NanoCPUs bound its CPU; MemoryBytes bounds its memory.
	Threads     int
	NanoCPUs    int64
	MemoryBytes int64
	// Context is the context size in tokens (default 12288, which fits
	// a distill.ChunkChars part, the prompt and the answer).
	Context int
	Labels  map[string]string
}

// Build returns the container spec.
func (d DistillSpec) Build() docker.Spec {
	img := d.Image
	if img == "" {
		img = DistillImage
	}
	ctxSize := d.Context
	if ctxSize <= 0 {
		ctxSize = 12288
	}
	threads := max(d.Threads, 1)
	hc := sandbox.Baseline(sandbox.BaselineOptions{
		NetworkMode: "none",
		TmpfsHome:   true,
		Limits:      sandbox.Limits{MemoryBytes: d.MemoryBytes, NanoCPUs: d.NanoCPUs, Pids: 256, NoFile: 4096},
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: d.ModelsDir, Target: distillModels, ReadOnly: true},
			{Type: mount.TypeBind, Source: d.SocketDir, Target: distillSock},
		},
	})
	return docker.Spec{
		Name:  d.Name,
		Image: img,
		User:  d.User,
		Cmd: []string{
			"-m", distillModels + "/" + d.ModelFile,
			"--host", distillSock + "/" + DistillSocketName,
			"-c", strconv.Itoa(ctxSize), "-t", strconv.Itoa(threads), "-np", "1",
			"--no-webui", "--log-disable",
		},
		Labels:     d.Labels,
		HostConfig: hc,
	}
}

// DistillMemory sizes the server's memory limit from its model file: the
// weights, a context's KV cache and headroom.
func DistillMemory(modelBytes int64) int64 { return modelBytes + 2<<30 }

// String describes the spec for logs.
func (d DistillSpec) String() string {
	return fmt.Sprintf("%s model=%s threads=%d", d.Name, d.ModelFile, d.Threads)
}

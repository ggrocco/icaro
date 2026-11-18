// Package distill is the small local model behind assistant memory (ADR
// 0036): a catalog of pinned GGUF models, their download, a client for an
// OpenAI-compatible endpoint (llama-server, Ollama), and the summary it
// asks for, constrained to a JSON schema. It has no Go dependency beyond
// the standard library; the model runs in llama.cpp, outside the daemon.
package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Model is one pinned GGUF file.
type Model struct {
	Name string `json:"name"`
	// Repo and File locate it on Hugging Face.
	Repo string `json:"repo"`
	File string `json:"file"`
	// SHA256 pins the file; a download that does not match is discarded.
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Tier    string `json:"tier"`
	License string `json:"license"`
}

// Tiers, smallest first.
const (
	TierMinimal  = "minimal"   // under 8 GB of RAM, no GPU
	TierCPU      = "cpu"       // 8 GB of RAM or more, no GPU
	TierGPU      = "gpu"       // a 6 GB GPU or Apple Silicon, through an endpoint
	TierLargeGPU = "large-gpu" // a 10 GB GPU, through an endpoint
)

// Catalog lists the models `icaro memory model pull` knows. There are no
// official GGUF builds of the Qwen3.5 small models; these are the
// community conversions the catalog was checked against, each Apache 2.0
// like its base model. The managed runtime is CPU only (ADR 0036), so the
// GPU tiers are for an endpoint the user runs.
var Catalog = []Model{
	{Name: "qwen3.5-0.8b", Repo: "unsloth/Qwen3.5-0.8B-GGUF", File: "Qwen3.5-0.8B-Q4_K_M.gguf",
		SHA256: "bd258782e35f7f458f8aced1adc053e6e92e89bc735ba3be89d38a06121dc517", Size: 532517120, Tier: TierMinimal, License: "apache-2.0"},
	{Name: "granite-4.0-1b", Repo: "ibm-granite/granite-4.0-1b-GGUF", File: "granite-4.0-1b-Q4_K_M.gguf",
		SHA256: "22ec0f9cc99a90185312de3c882c84e7bd6789bdd050389844380a01a831d7f1", Size: 1023645440, Tier: TierMinimal, License: "apache-2.0"},
	{Name: "qwen3.5-2b", Repo: "unsloth/Qwen3.5-2B-GGUF", File: "Qwen3.5-2B-Q4_K_M.gguf",
		SHA256: "aaf42c8b7c3cab2bf3d69c355048d4a0ee9973d48f16c731c0520ee914699223", Size: 1280835840, Tier: TierCPU, License: "apache-2.0"},
	{Name: "qwen3.5-4b", Repo: "unsloth/Qwen3.5-4B-GGUF", File: "Qwen3.5-4B-Q4_K_M.gguf",
		SHA256: "00fe7986ff5f6b463e62455821146049db6f9313603938a70800d1fb69ef11a4", Size: 2740937888, Tier: TierGPU, License: "apache-2.0"},
	{Name: "gemma-4-e4b", Repo: "unsloth/gemma-4-E4B-it-GGUF", File: "gemma-4-E4B-it-Q4_K_M.gguf",
		SHA256: "85a896a047553e842f25297ee5b031d64ff30147d9c4af17b1e4b394cd1fab87", Size: 4977171584, Tier: TierGPU, License: "apache-2.0"},
	{Name: "qwen3.5-9b", Repo: "unsloth/Qwen3.5-9B-GGUF", File: "Qwen3.5-9B-Q4_K_M.gguf",
		SHA256: "03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8", Size: 5680522464, Tier: TierLargeGPU, License: "apache-2.0"},
}

// ErrUnknownModel reports a name the catalog does not have.
var ErrUnknownModel = errors.New("unknown model")

// Lookup returns a catalog model by name.
func Lookup(name string) (Model, error) {
	for _, m := range Catalog {
		if m.Name == name {
			return m, nil
		}
	}
	names := make([]string, len(Catalog))
	for i, m := range Catalog {
		names[i] = m.Name
	}
	return Model{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownModel, name, strings.Join(names, ", "))
}

// gib is 2^30 bytes.
const gib = int64(1) << 30

// AutoTier is the managed runtime's tier for a machine's RAM. The managed
// runtime is CPU only, so GPU tiers are never picked here.
func AutoTier(ramBytes int64) string {
	if ramBytes > 0 && ramBytes < 8*gib {
		return TierMinimal
	}
	return TierCPU
}

// Default is the model `pull` fetches for a tier: its first catalog entry.
func Default(tier string) Model {
	for _, m := range Catalog {
		if m.Tier == tier {
			return m
		}
	}
	return Catalog[0]
}

// Store manages downloaded models in one directory.
type Store struct {
	Dir string
	// HTTP downloads; nil uses http.DefaultClient.
	HTTP *http.Client
	// BaseURL is the Hugging Face host (tests point it elsewhere).
	BaseURL string
}

// Path is where a model's file lives.
func (s Store) Path(m Model) string { return filepath.Join(s.Dir, m.File) }

// Installed lists the catalog models present, smallest first. A file is
// trusted by its size here; Verify checks its hash.
func (s Store) Installed() []Model {
	var out []Model
	for _, m := range Catalog {
		if fi, err := os.Stat(s.Path(m)); err == nil && fi.Size() == m.Size {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	return out
}

// Pick returns the installed model to run: the named one, or for "auto"
// (or "") the largest installed model of the tier, falling back to any
// installed model. ok is false when none is installed.
func (s Store) Pick(name, tier string) (Model, bool, error) {
	installed := s.Installed()
	if name != "" && name != "auto" {
		m, err := Lookup(name)
		if err != nil {
			return Model{}, false, err
		}
		for _, i := range installed {
			if i.Name == m.Name {
				return m, true, nil
			}
		}
		return m, false, nil
	}
	var best Model
	found := false
	for _, m := range installed {
		if m.Tier == tier {
			best, found = m, true
		}
	}
	if !found && len(installed) > 0 {
		best, found = installed[0], true
	}
	return best, found, nil
}

// ErrHashMismatch reports a download whose SHA-256 is not the pinned one.
var ErrHashMismatch = errors.New("the download does not match the pinned SHA-256")

// Pull downloads m, checks its SHA-256 and moves it into place. A file
// already in place with the right size is checked and kept.
func (s Store) Pull(ctx context.Context, m Model, progress func(done, total int64)) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Stat(s.Path(m)); err == nil && fi.Size() == m.Size {
		return s.Verify(m)
	}
	base := s.BaseURL
	if base == "" {
		base = "https://huggingface.co"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+m.Repo+"/resolve/main/"+m.File, nil)
	if err != nil {
		return err
	}
	hc := s.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", m.Name, resp.Status)
	}
	part := s.Path(m) + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h, &counter{total: m.Size, fn: progress}), io.LimitReader(resp.Body, m.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		err = fmt.Errorf("%w (%s)", ErrHashMismatch, m.Name)
	}
	if err != nil {
		_ = os.Remove(part)
		return err
	}
	return os.Rename(part, s.Path(m))
}

// Verify checks an installed model against its pinned SHA-256.
func (s Store) Verify(m Model) error {
	f, err := os.Open(s.Path(m))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return fmt.Errorf("%w (%s)", ErrHashMismatch, m.Name)
	}
	return nil
}

// Remove deletes an installed model.
func (s Store) Remove(m Model) error {
	err := os.Remove(s.Path(m))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("model %s is not installed", m.Name)
	}
	return err
}

type counter struct {
	done, total int64
	fn          func(done, total int64)
}

func (c *counter) Write(p []byte) (int, error) {
	c.done += int64(len(p))
	if c.fn != nil {
		c.fn(c.done, c.total)
	}
	return len(p), nil
}

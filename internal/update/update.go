// Package update implements `icaro update` (Phase 8): it finds the latest
// release, downloads only signed and checksummed artifacts, stages the new
// binary next to the current one, smoke-tests it, swaps it in atomically
// and keeps the previous binary for `icaro update --rollback` (ADR 0021).
//
// It never touches the state root (~/.icaro): config and the database are
// preserved by construction, and schema changes are applied by the new
// binary's migrations when the daemon restarts.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultPublicKey is the release signing key (base64 ed25519), set at
// build time with -ldflags "-X icaro/internal/update.DefaultPublicKey=...".
// config update.public_key overrides it.
var DefaultPublicKey = ""

// DefaultRepo is the GitHub repository releases come from.
const DefaultRepo = "ggrocco/icaro"

// Release files.
const (
	ChecksumsFile = "checksums.txt"
	SignatureFile = "checksums.txt.sig"
	binaryName    = "icaro"
	maxDownload   = 256 << 20
)

// Release locates one version's artifacts: each lives at BaseURL/<name>.
type Release struct {
	Version string
	BaseURL string
}

// Source finds releases.
type Source interface {
	Latest(ctx context.Context) (Release, error)
	At(version string) Release
}

// GitHub reads releases from the GitHub API and downloads assets from
// github.com.
type GitHub struct {
	Repo        string
	APIURL      string // default https://api.github.com
	DownloadURL string // default https://github.com
	HTTP        *http.Client
}

func (g *GitHub) api() string {
	return strings.TrimRight(orDefault(g.APIURL, "https://api.github.com"), "/")
}
func (g *GitHub) dl() string {
	return strings.TrimRight(orDefault(g.DownloadURL, "https://github.com"), "/")
}

// Latest returns the newest published (non-draft, non-prerelease) release.
func (g *GitHub) Latest(ctx context.Context) (Release, error) {
	var body struct {
		TagName string `json:"tag_name"`
	}
	data, err := get(ctx, g.HTTP, g.api()+"/repos/"+g.Repo+"/releases/latest", 1<<20)
	if err != nil {
		return Release{}, err
	}
	if err := json.Unmarshal(data, &body); err != nil || body.TagName == "" {
		return Release{}, fmt.Errorf("unexpected GitHub release response")
	}
	return g.At(body.TagName), nil
}

// At returns the release for a tag such as v1.2.3.
func (g *GitHub) At(version string) Release {
	tag := "v" + strings.TrimPrefix(version, "v")
	return Release{Version: tag, BaseURL: g.dl() + "/" + g.Repo + "/releases/download/" + tag}
}

// Mirror serves releases from a static base URL laid out as
// <base>/latest (a file holding the tag) and <base>/<tag>/<asset>.
type Mirror struct {
	BaseURL string
	HTTP    *http.Client
}

// Latest reads <base>/latest.
func (m *Mirror) Latest(ctx context.Context) (Release, error) {
	data, err := get(ctx, m.HTTP, strings.TrimRight(m.BaseURL, "/")+"/latest", 256)
	if err != nil {
		return Release{}, err
	}
	tag := strings.TrimSpace(string(data))
	if tag == "" || strings.ContainsAny(tag, "/ \n") {
		return Release{}, fmt.Errorf("invalid latest tag %q", tag)
	}
	return m.At(tag), nil
}

// At returns the release for a tag.
func (m *Mirror) At(version string) Release {
	tag := "v" + strings.TrimPrefix(version, "v")
	return Release{Version: tag, BaseURL: strings.TrimRight(m.BaseURL, "/") + "/" + tag}
}

// NewSource picks a Mirror when baseURL is set, else GitHub.
func NewSource(baseURL string, hc *http.Client) Source {
	if baseURL != "" {
		return &Mirror{BaseURL: baseURL, HTTP: hc}
	}
	return &GitHub{Repo: DefaultRepo, HTTP: hc}
}

// ArchiveName is the goreleaser archive name for a version and platform.
func ArchiveName(version, goos, goarch string) string {
	return fmt.Sprintf("icaro_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), goos, goarch)
}

// Updater replaces the running binary.
type Updater struct {
	Exe       string // path of the binary to replace
	Current   string // running version
	Source    Source
	PublicKey ed25519.PublicKey
	HTTP      *http.Client
	GOOS      string
	GOARCH    string
	// Smoke runs the staged binary; default executes "<path> version".
	Smoke func(ctx context.Context, path string) (string, error)
}

// Plan is the result of a check.
type Plan struct {
	Current   string  `json:"current"`
	Latest    string  `json:"latest"`
	Available bool    `json:"available"`
	Release   Release `json:"-"`
}

// Check compares the running version with the latest release.
func (u *Updater) Check(ctx context.Context) (Plan, error) {
	rel, err := u.Source.Latest(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("find latest release: %w", err)
	}
	return Plan{Current: u.Current, Latest: rel.Version, Available: Newer(rel.Version, u.Current), Release: rel}, nil
}

// StagedPath is where the new binary is written before the swap.
func StagedPath(exe string) string { return exe + ".new" }

// PrevPath is where the previous binary is kept for rollback.
func PrevPath(exe string) string { return exe + ".prev" }

// Apply downloads, verifies, stages, smoke-tests and swaps in rel. Any
// verification failure leaves the current binary untouched.
func (u *Updater) Apply(ctx context.Context, rel Release) error {
	if u.PublicKey == nil {
		return errors.New("no release public key: refusing to install an unsigned update")
	}
	goos, goarch := orDefault(u.GOOS, runtime.GOOS), orDefault(u.GOARCH, runtime.GOARCH)
	sums, err := u.fetch(ctx, rel, ChecksumsFile, 1<<20)
	if err != nil {
		return err
	}
	sig, err := u.fetch(ctx, rel, SignatureFile, 4096)
	if err != nil {
		return err
	}
	if err := VerifySignature(u.PublicKey, sums, sig); err != nil {
		return err
	}
	table, err := ParseChecksums(sums)
	if err != nil {
		return err
	}
	name := ArchiveName(rel.Version, goos, goarch)
	archive, err := u.fetch(ctx, rel, name, maxDownload)
	if err != nil {
		return err
	}
	if err := VerifyChecksum(table, name, archive); err != nil {
		return err
	}
	bin, err := extractBinary(archive)
	if err != nil {
		return err
	}
	staged := StagedPath(u.Exe)
	if err := os.WriteFile(staged, bin, 0o755); err != nil { //nolint:gosec // executable
		return fmt.Errorf("stage: %w", err)
	}
	smoke := u.Smoke
	if smoke == nil {
		smoke = runVersion
	}
	if out, err := smoke(ctx, staged); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("staged binary failed its smoke test: %v (%s)", err, strings.TrimSpace(out))
	}
	return swap(u.Exe, staged)
}

func (u *Updater) fetch(ctx context.Context, rel Release, name string, limit int64) ([]byte, error) {
	data, err := get(ctx, u.HTTP, rel.BaseURL+"/"+path.Base(name), limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return data, nil
}

// swap keeps the current binary as .prev (hard link, else copy) and then
// renames the staged binary over it, which is atomic on one filesystem.
func swap(exe, staged string) error {
	prev := PrevPath(exe)
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		if err := copyFile(exe, prev); err != nil {
			return fmt.Errorf("keep previous binary: %w", err)
		}
	}
	if err := os.Rename(staged, exe); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	return nil
}

// Rollback restores the binary kept by the last update.
func Rollback(exe string) error {
	prev := PrevPath(exe)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("nothing to roll back to (%s): %w", prev, err)
	}
	return os.Rename(prev, exe)
}

// Executable returns the real path of the running binary.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func runVersion(ctx context.Context, p string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "version").CombinedOutput() //nolint:gosec // our staged binary
	return string(out), err
}

func extractBinary(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no icaro binary")
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == binaryName {
			if h.Size > maxDownload {
				return nil, errors.New("binary too large")
			}
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

func get(ctx context.Context, hc *http.Client, url string, limit int64) ([]byte, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "icaro-update")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, limit)
	}
	return data, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Newer reports whether latest is a newer version than current. A
// non-release current version (dev, a git hash) is always older.
func Newer(latest, current string) bool {
	l, ok := parseVersion(latest)
	if !ok {
		return false
	}
	c, ok := parseVersion(current)
	if !ok {
		return true
	}
	for i := 0; i < 3; i++ {
		if l.nums[i] != c.nums[i] {
			return l.nums[i] > c.nums[i]
		}
	}
	switch {
	case l.pre == c.pre:
		return false
	case l.pre == "":
		return true // a release beats its prereleases
	case c.pre == "":
		return false
	}
	return l.pre > c.pre
}

type version struct {
	nums [3]int
	pre  string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v version
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		if s[i] == '-' {
			v.pre = s[i+1:]
			if j := strings.IndexByte(v.pre, '+'); j >= 0 {
				v.pre = v.pre[:j]
			}
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.nums[i] = n
	}
	return v, true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

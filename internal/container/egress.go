package container

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/mount"

	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
)

// Egress labels and names.
const (
	LabelEgress     = "icaro.egress"
	LabelEgressHash = "icaro.egress.hash"
	// ProxyAlias is the proxy's DNS name on every profile network.
	ProxyAlias = "icaro-egress"
	// ProxyPort is where the proxy listens.
	ProxyPort = 3128
	// InjectPort is where a proxy relays credential injection requests to
	// the daemon (ADR 0028).
	InjectPort = 3129
	// InjectMount is where a proxy container sees the directory of the
	// daemon's injection socket; InjectSocketName is the socket in it.
	InjectMount      = "/icaro-inject"
	InjectSocketName = "inject.sock"
)

// InjectSocketDir is the host directory of a profile's injection socket.
// Only that profile's proxy container mounts it; agent containers never do.
func InjectSocketDir(runDir, profile string) string {
	return filepath.Join(runDir, "inject", profile)
}

// NetworkName is the internal network of a profile (no namespace).
func NetworkName(profile string) string { return "icaro-egress-" + profile }

// Namespace derives a short egress namespace from a state root, so two
// Ícaro homes on one Docker engine keep separate networks and proxies
// even when they use the same profile name with different allowlists.
func Namespace(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(sum[:4])
}

// DockerEgress implements Egress with one internal network and one proxy
// container per profile (ADR 0005). Containers on the network have no
// route out; the proxy, also attached to the default bridge, forwards
// only to allow-listed hosts.
type DockerEgress struct {
	Engine Engine
	// HelperImage runs `icaro egress-proxy`; empty means build one from
	// HelperBinary with `docker import` (Linux, static binary only).
	HelperImage  string
	HelperBinary string
	// OutsideNetwork is the network the proxy uses to reach the world.
	OutsideNetwork string
	// Namespace scopes network and proxy names and labels (see Namespace);
	// empty keeps the bare profile names.
	Namespace string
	// InjectDir returns the host directory of a profile's credential
	// injection socket (InjectSocketDir), or "" when the profile injects
	// nothing (ADR 0028). The proxy of such a profile mounts it read-only
	// and relays InjectPort to it.
	InjectDir func(profile string) string
	// InjectUser runs a proxy that relays injection: it must be able to
	// connect to the daemon's 0600 socket. Empty keeps 65534:65534.
	InjectUser string
	Log        *slog.Logger
}

// network is the internal network name for a profile.
func (e *DockerEgress) network(profile string) string {
	if e.Namespace == "" {
		return NetworkName(profile)
	}
	return "icaro-egress-" + e.Namespace + "-" + profile
}

// key is the label value identifying a profile's egress resources.
func (e *DockerEgress) key(profile string) string {
	if e.Namespace == "" {
		return profile
	}
	return e.Namespace + "/" + profile
}

// Ensure provisions the network and, for a non-empty allowlist or a
// profile that injects credentials, the proxy.
func (e *DockerEgress) Ensure(ctx context.Context, profile string, allow []string) (Attachment, error) {
	netName := e.network(profile)
	labels := map[string]string{LabelEgress: e.key(profile)}
	if err := e.Engine.CreateNetwork(ctx, netName, true, labels); err != nil {
		return Attachment{}, fmt.Errorf("egress network: %w", err)
	}
	att := Attachment{Network: netName}
	injectDir := ""
	if e.InjectDir != nil {
		injectDir = e.InjectDir(profile)
	}
	if len(allow) == 0 && injectDir == "" {
		return att, nil // no network access at all
	}
	if injectDir != "" {
		// Create it before Docker would, as root, for the bind mount.
		if err := os.MkdirAll(injectDir, 0o700); err != nil {
			return Attachment{}, fmt.Errorf("injection socket directory: %w", err)
		}
	}
	image, err := e.helperImage(ctx)
	if err != nil {
		return Attachment{}, err
	}
	sorted := append([]string(nil), allow...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",") + "|" + image + "|" + injectDir + "|" + e.InjectUser))
	hash := hex.EncodeToString(sum[:6])

	ids, err := e.Engine.ListByLabel(ctx, map[string]string{LabelEgress: e.key(profile)})
	if err != nil {
		return Attachment{}, err
	}
	reuse := ""
	for _, id := range ids {
		st, err := e.Engine.Inspect(ctx, id)
		if err != nil {
			return Attachment{}, err
		}
		if st.Exists && st.Running && st.Labels[LabelEgressHash] == hash && reuse == "" {
			reuse = id
			continue
		}
		_ = e.Engine.RemoveContainer(ctx, id)
	}
	proxyID := reuse
	if proxyID == "" {
		if proxyID, err = e.startProxy(ctx, profile, image, hash, sorted, injectDir); err != nil {
			return Attachment{}, err
		}
	}
	st, err := e.Engine.Inspect(ctx, proxyID)
	if err != nil {
		return Attachment{}, fmt.Errorf("inspect egress proxy: %w", err)
	}
	if ip := st.IPs[netName]; ip != "" {
		att.Hosts = []string{ProxyAlias + ":" + ip}
	}
	noProxy := "localhost,127.0.0.1"
	if injectDir != "" {
		// The injection endpoint is reached directly, not through the proxy.
		noProxy += "," + ProxyAlias
		att.Inject = fmt.Sprintf("http://%s:%d", ProxyAlias, InjectPort)
	}
	if len(allow) > 0 {
		proxy := fmt.Sprintf("http://%s:%d", ProxyAlias, ProxyPort)
		att.Env = []string{"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy}
	}
	att.Env = append(att.Env, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	return att, nil
}

// startProxy creates and starts the proxy of profile. With injectDir it
// also relays InjectPort to the daemon's socket in that directory; with an
// empty allowlist it then sits on the internal network only, since it
// forwards nothing outside.
func (e *DockerEgress) startProxy(ctx context.Context, profile, image, hash string, allow []string, injectDir string) (string, error) {
	outside := e.OutsideNetwork
	if outside == "" {
		outside = "bridge"
	}
	first := outside
	if len(allow) == 0 {
		first = e.network(profile)
	}
	opts := sandbox.BaselineOptions{
		NetworkMode: first,
		TmpfsHome:   true,
		Limits:      sandbox.Limits{MemoryBytes: 128 << 20, NanoCPUs: 5e8, Pids: 128, NoFile: 4096},
	}
	entry := []string{"/icaro", "egress-proxy", "--listen", fmt.Sprintf(":%d", ProxyPort), "--allow", strings.Join(allow, ",")}
	user := "65534:65534"
	if injectDir != "" {
		opts.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: injectDir, Target: InjectMount, ReadOnly: true}}
		entry = append(entry, "--inject-listen", fmt.Sprintf(":%d", InjectPort), "--inject-socket", InjectMount+"/"+InjectSocketName)
		if e.InjectUser != "" {
			user = e.InjectUser
		}
	}
	hc := sandbox.Baseline(opts)
	hc.RestartPolicy.Name = "unless-stopped"
	spec := docker.Spec{
		Name:       e.network(profile) + "-proxy-" + hash,
		Image:      image,
		Entrypoint: entry,
		User:       user,
		Labels:     map[string]string{LabelEgress: e.key(profile), LabelEgressHash: hash},
		HostConfig: hc,
	}
	ns := docker.NetSpec{Network: outside}
	if len(allow) == 0 {
		ns = docker.NetSpec{Network: first, Aliases: []string{ProxyAlias}}
	}
	id, err := e.Engine.CreateContainerOn(ctx, spec, ns)
	if err != nil {
		return "", fmt.Errorf("create egress proxy: %w", err)
	}
	if len(allow) > 0 {
		if err := e.Engine.ConnectNetwork(ctx, e.network(profile), id, []string{ProxyAlias}); err != nil {
			_ = e.Engine.RemoveContainer(ctx, id)
			return "", fmt.Errorf("attach egress proxy: %w", err)
		}
	}
	if err := e.Engine.StartContainer(ctx, id); err != nil {
		_ = e.Engine.RemoveContainer(ctx, id)
		return "", fmt.Errorf("start egress proxy: %w", err)
	}
	if e.Log != nil {
		e.Log.Info("egress proxy started", "profile", profile, "allow", strings.Join(allow, ","), "inject", injectDir != "")
	}
	return id, nil
}

// helperImage returns the image that contains /icaro.
func (e *DockerEgress) helperImage(ctx context.Context) (string, error) {
	if e.HelperImage != "" {
		return e.HelperImage, e.Engine.EnsureImage(ctx, e.HelperImage)
	}
	bin := e.HelperBinary
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		bin = exe
	}
	ref, data, err := HelperImageFromBinary(bin)
	if err != nil {
		return "", err
	}
	if _, err := e.Engine.ImageDigest(ctx, ref); err == nil {
		return ref, nil
	}
	if err := e.Engine.ImportImage(ctx, bytes.NewReader(data), ref, nil); err != nil {
		return "", fmt.Errorf("import helper image: %w", err)
	}
	return ref, nil
}

// ErrNoHelper reports that no helper image can be built from this binary.
var ErrNoHelper = errors.New("cannot build the icaro helper image from this binary; set docker.helper_image (e.g. the agent base image)")

// HelperImageFromBinary returns the helper image name for a static Linux
// binary and the root filesystem tarball holding it at /icaro.
func HelperImageFromBinary(path string) (string, []byte, error) {
	if runtime.GOOS != "linux" {
		return "", nil, ErrNoHelper
	}
	f, err := elf.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrNoHelper, err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			_ = f.Close()
			return "", nil, fmt.Errorf("%w: %s is dynamically linked (build with CGO_ENABLED=0)", ErrNoHelper, path)
		}
	}
	_ = f.Close()
	bin, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(bin)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, d := range []string{"tmp/", "etc/"} {
		_ = tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	passwd := []byte("nobody:x:65534:65534:nobody:/tmp:/sbin/nologin\n")
	_ = tw.WriteHeader(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(passwd))})
	_, _ = tw.Write(passwd)
	if err := tw.WriteHeader(&tar.Header{Name: "icaro", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bin))}); err != nil {
		return "", nil, err
	}
	if _, err := io.Copy(tw, bytes.NewReader(bin)); err != nil {
		return "", nil, err
	}
	if err := tw.Close(); err != nil {
		return "", nil, err
	}
	return "icaro-helper:" + hex.EncodeToString(sum[:6]), buf.Bytes(), nil
}

var _ Egress = (*DockerEgress)(nil)

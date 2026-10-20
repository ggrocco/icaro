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
	"runtime"
	"sort"
	"strings"

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
)

// NetworkName is the internal network of a profile.
func NetworkName(profile string) string { return "icaro-egress-" + profile }

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
	Log            *slog.Logger
}

// Ensure provisions the network and (for a non-empty allowlist) the proxy.
func (e *DockerEgress) Ensure(ctx context.Context, profile string, allow []string) (Attachment, error) {
	netName := NetworkName(profile)
	labels := map[string]string{LabelEgress: profile}
	if err := e.Engine.CreateNetwork(ctx, netName, true, labels); err != nil {
		return Attachment{}, fmt.Errorf("egress network: %w", err)
	}
	att := Attachment{Network: netName}
	if len(allow) == 0 {
		return att, nil // no network access at all
	}
	image, err := e.helperImage(ctx)
	if err != nil {
		return Attachment{}, err
	}
	sorted := append([]string(nil), allow...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",") + "|" + image))
	hash := hex.EncodeToString(sum[:6])

	ids, err := e.Engine.ListByLabel(ctx, map[string]string{LabelEgress: profile})
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
	if reuse == "" {
		if err := e.startProxy(ctx, profile, image, hash, sorted); err != nil {
			return Attachment{}, err
		}
	}
	proxy := fmt.Sprintf("http://%s:%d", ProxyAlias, ProxyPort)
	att.Env = []string{
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
	}
	return att, nil
}

func (e *DockerEgress) startProxy(ctx context.Context, profile, image, hash string, allow []string) error {
	outside := e.OutsideNetwork
	if outside == "" {
		outside = "bridge"
	}
	hc := sandbox.Baseline(sandbox.BaselineOptions{
		NetworkMode: outside,
		TmpfsHome:   true,
		Limits:      sandbox.Limits{MemoryBytes: 128 << 20, NanoCPUs: 5e8, Pids: 128, NoFile: 4096},
	})
	hc.RestartPolicy.Name = "unless-stopped"
	spec := docker.Spec{
		Name:       NetworkName(profile) + "-proxy-" + hash,
		Image:      image,
		Entrypoint: []string{"/icaro", "egress-proxy", "--listen", fmt.Sprintf(":%d", ProxyPort), "--allow", strings.Join(allow, ",")},
		User:       "65534:65534",
		Labels:     map[string]string{LabelEgress: profile, LabelEgressHash: hash},
		HostConfig: hc,
	}
	id, err := e.Engine.CreateContainerOn(ctx, spec, docker.NetSpec{Network: outside})
	if err != nil {
		return fmt.Errorf("create egress proxy: %w", err)
	}
	if err := e.Engine.ConnectNetwork(ctx, NetworkName(profile), id, []string{ProxyAlias}); err != nil {
		_ = e.Engine.RemoveContainer(ctx, id)
		return fmt.Errorf("attach egress proxy: %w", err)
	}
	if err := e.Engine.StartContainer(ctx, id); err != nil {
		_ = e.Engine.RemoveContainer(ctx, id)
		return fmt.Errorf("start egress proxy: %w", err)
	}
	if e.Log != nil {
		e.Log.Info("egress proxy started", "profile", profile, "allow", strings.Join(allow, ","))
	}
	return nil
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

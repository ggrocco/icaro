package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"

	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/creds"
	"icaro/internal/doctor"
	"icaro/internal/store"
)

// profileProblems lists why a profile's agent cannot run unattended: not
// signed in (unless it injects an API key), or an allowlist that does not
// reach the provider. Shared by the assistant's and agent steps' checks.
func profileProblems(cfg *config.Config, name string, prof config.ProfileConfig, agent creds.Agent) []string {
	if len(prof.Inject) > 0 {
		return nil
	}
	var probs []string
	seed := (&creds.Homes{Layout: cfg.Layout()}).SeedDir(name)
	signedIn := false
	for _, rel := range assistantLogins[agent.Name] {
		signedIn = signedIn || fileExists(filepath.Join(seed, rel))
	}
	if !signedIn {
		probs = append(probs, fmt.Sprintf("profile %s is not signed in; run `icaro login %s`", name, name))
	}
	for _, host := range assistantHosts[agent.Name] {
		if !slices.Contains(prof.Egress, host) {
			probs = append(probs, fmt.Sprintf("profile %s's egress allowlist does not reach %s", name, host))
		}
	}
	return probs
}

// CLIVersions reports the agent CLI version inside an image. It returns
// errNotPulled when the image is not present locally (doctor never pulls).
type CLIVersions func(ctx context.Context, image string, cmd []string) (string, error)

var errNotPulled = errors.New("the image is not pulled yet")

// AgentStepsCheck reports whether each profile in workflows.agent_profiles
// can run agent steps (ADR 0037): signed in or injecting a key, an
// allowlist that reaches the provider, and an image whose CLI is at least
// the catalog's minimum version. versions may be nil (no Docker).
func AgentStepsCheck(cfg *config.Config, versions CLIVersions) doctor.Check {
	return doctor.Func("agent steps", func(ctx context.Context) doctor.Result {
		names := cfg.Workflows.AgentProfiles
		if len(names) == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: "agent steps are off: workflows.agent_profiles is empty"}
		}
		var probs, oks []string
		for _, name := range names {
			prof := cfg.Profiles[name]
			agent, err := creds.LookupAgent(prof.Agent)
			if err != nil {
				probs = append(probs, fmt.Sprintf("profile %s: %v", name, err))
				continue
			}
			spec, err := agent.Step()
			if err != nil {
				probs = append(probs, fmt.Sprintf("profile %s: %v", name, err))
				continue
			}
			p := profileProblems(cfg, name, prof, agent)
			if versions != nil {
				out, err := versions(ctx, prof.Image, spec.VersionCommand)
				switch {
				case errors.Is(err, errNotPulled):
					oks = append(oks, fmt.Sprintf("profile %s: image %s is not pulled yet, CLI version not checked", name, prof.Image))
				case err != nil:
					p = append(p, fmt.Sprintf("profile %s: cannot run %s in %s: %v", name, strings.Join(spec.VersionCommand, " "), prof.Image, err))
				default:
					v := versionIn(out)
					if v == "" {
						p = append(p, fmt.Sprintf("profile %s: %s printed no version (%q)", name, strings.Join(spec.VersionCommand, " "), strings.TrimSpace(out)))
					} else if versionLess(v, spec.MinVersion) {
						p = append(p, fmt.Sprintf("profile %s: %s %s is older than %s, the oldest the step mode supports; update the image", name, agent.Name, v, spec.MinVersion))
					}
				}
			}
			if len(p) == 0 {
				oks = append(oks, fmt.Sprintf("profile %s (%s) is ready", name, agent.Name))
			}
			probs = append(probs, p...)
		}
		if len(probs) > 0 {
			return doctor.Result{Severity: doctor.Warning, Detail: strings.Join(probs, "; ")}
		}
		return doctor.Result{Severity: doctor.OK, Detail: strings.Join(oks, "; ")}
	})
}

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// versionIn finds the first x.y[.z] in a CLI's version output.
func versionIn(s string) string { return versionRe.FindString(s) }

// versionLess compares dotted numeric versions.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// AgentStepsChecks is the CheckFactory form of AgentStepsCheck, for the
// daemon and for `icaro doctor` without a daemon.
func AgentStepsChecks(_ context.Context, cfg *config.Config, _ *store.Store, eng *docker.Engine) []doctor.Check {
	var versions CLIVersions
	if eng != nil {
		versions = dockerVersions(eng)
	}
	return []doctor.Check{AgentStepsCheck(cfg, versions)}
}

// cliProbe is the Docker surface dockerVersions uses.
type cliProbe interface {
	ImageDigest(ctx context.Context, ref string) (string, error)
	CreateContainer(ctx context.Context, spec docker.Spec) (string, error)
	StartContainer(ctx context.Context, id string) error
	Wait(ctx context.Context, id string) (int, error)
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	RemoveContainer(ctx context.Context, id string) error
}

// dockerVersions runs the version command in a throwaway container under
// the baseline with no network, if the image is present locally.
func dockerVersions(eng cliProbe) CLIVersions {
	return func(ctx context.Context, image string, cmd []string) (string, error) {
		if _, err := eng.ImageDigest(ctx, image); errors.Is(err, docker.ErrNotFound) {
			return "", errNotPulled
		} else if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		hc := sandbox.Baseline(sandbox.BaselineOptions{NetworkMode: "none", TmpfsHome: true})
		id, err := eng.CreateContainer(ctx, docker.Spec{Image: image, Entrypoint: cmd, HostConfig: hc})
		if err != nil {
			return "", err
		}
		defer func() { _ = eng.RemoveContainer(context.WithoutCancel(ctx), id) }()
		if err := eng.StartContainer(ctx, id); err != nil {
			return "", err
		}
		if _, err := eng.Wait(ctx, id); err != nil {
			return "", err
		}
		rc, err := eng.Logs(ctx, id, false)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		var out bytes.Buffer
		_, _ = stdcopy.StdCopy(&out, &out, rc)
		return out.String(), nil
	}
}

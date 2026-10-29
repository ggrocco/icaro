package container

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"icaro/internal/doctor"
)

// RuntimeEngine is what the runtime check needs (docker.Engine).
type RuntimeEngine interface {
	Ping(ctx context.Context) error
	Runtimes(ctx context.Context) ([]string, error)
}

// RuntimeCheck reports every profile whose OCI runtime (profiles.<name>.
// runtime, e.g. runsc for gVisor) the engine has not registered: the
// profile's sessions, and the steps of runs on it, would fail to start.
// runtimes maps a profile to its runtime; profiles on Docker's default
// runtime are left out.
func RuntimeCheck(e RuntimeEngine, runtimes map[string]string) doctor.Check {
	return doctor.Func("container runtimes", func(ctx context.Context) doctor.Result {
		if len(runtimes) == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: "every profile uses Docker's default runtime"}
		}
		if err := e.Ping(ctx); err != nil {
			return doctor.Result{Severity: doctor.Skipped, Detail: "docker unreachable"}
		}
		registered, err := e.Runtimes(ctx)
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		}
		profiles := make([]string, 0, len(runtimes))
		for p := range runtimes {
			profiles = append(profiles, p)
		}
		slices.Sort(profiles)
		var ok, missing []string
		for _, p := range profiles {
			rt := runtimes[p]
			if slices.Contains(registered, rt) {
				ok = append(ok, p+": "+rt)
			} else {
				missing = append(missing, fmt.Sprintf("%s uses %q", p, rt))
			}
		}
		if len(missing) > 0 {
			return doctor.Result{Severity: doctor.Error, Detail: fmt.Sprintf(
				"not registered with Docker, so these profiles cannot start containers: %s (registered: %s); install the runtime and register it in /etc/docker/daemon.json (gVisor: `sudo runsc install -- --host-uds=open` and restart Docker), or clear the profile's runtime",
				strings.Join(missing, ", "), strings.Join(registered, ", "))}
		}
		return doctor.Result{Severity: doctor.OK, Detail: strings.Join(ok, ", ")}
	})
}

package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// doctorTableChecks maps each row of the doctor table in
// blueprint/docs/03-security.md to the check that covers it. A new row
// without an entry here fails TestDoctorCoversSecurityTable.
var doctorTableChecks = map[string]string{
	"Docker missing/inactive":             "docker",
	"Orphaned lock/socket/PID":            "daemon lock/socket/pid",
	"Orphaned sessions":                   "orphaned sessions",
	"Insecure permissions on `~/.icaro`":  "permissions on ",
	"Pending or failed migrations":        "database migrations",
	"Unverified image":                    "pinned images",
	"Proxy without a dedicated network":   "preview proxy network",
	"Local DNS for `.test` misconfigured": "preview DNS",
	"Missing webhook secret":              "webhook secrets",
	"Dangerous compose":                   "preview compose lint",
}

func TestDoctorCoversSecurityTable(t *testing.T) {
	doc, err := os.ReadFile("../../blueprint/docs/03-security.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(doc), "## `icaro doctor`")
	if !ok {
		t.Fatal("doctor table not found")
	}
	var rows []string
	for _, line := range strings.Split(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") || strings.TrimSpace(cells[1]) == "Check" {
			continue
		}
		row, _, _ := strings.Cut(strings.TrimSpace(cells[1]), " (") // drop the parenthetical detail
		rows = append(rows, row)
	}
	if len(rows) < 10 {
		t.Fatalf("parsed only %d rows: %v", len(rows), rows)
	}

	home, err := os.MkdirTemp("/tmp", "icaro-doc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	checks, cleanup, err := doctorChecks(cmd, &rootOpts{home: home})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	for _, row := range rows {
		want, ok := doctorTableChecks[row]
		if !ok {
			t.Errorf("doctor table row %q has no check mapped in doctorTableChecks", row)
			continue
		}
		found := false
		for _, n := range names {
			if strings.HasPrefix(n, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("row %q: no registered check named %q (have %v)", row, want, names)
		}
	}
}

// TestDoctorUnbuildableDockerClient is the regression test for the typed-nil
// panic: a docker.host the client cannot parse used to crash doctor with a
// nil pointer dereference instead of reporting an error.
func TestDoctorUnbuildableDockerClient(t *testing.T) {
	daemonEnv(t)
	t.Setenv("ICARO_DOCKER__HOST", "notaurl")
	out, err := runCLI(t, "", "doctor")
	if err == nil || !strings.Contains(err.Error(), "doctor found errors") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "cannot build a Docker client") {
		t.Errorf("docker check does not report the client error:\n%s", out)
	}
}

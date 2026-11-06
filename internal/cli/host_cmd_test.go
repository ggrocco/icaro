package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRemote is another Ícaro daemon: sign-in and workflow listing.
func fakeRemote(t *testing.T, token string, workflows ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ui/me":
			_, _ = io.WriteString(w, `{"name":"ci","scope":"read"}`)
		case "/api/v1/workflows":
			var out []map[string]any
			for _, n := range workflows {
				out = append(out, map[string]any{"name": n, "version": 2, "status": "active", "steps": []string{"a"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": out})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loginHost(t *testing.T, token string, args ...string) (string, error) {
	t.Helper()
	restore := withStdio(t, token+"\n")
	defer restore()
	return runCLI(t, "", append([]string{"host", "login"}, args...)...)
}

func TestHostCommands(t *testing.T) {
	startDaemon(t)
	prod := fakeRemote(t, "tok-prod", "deploy")
	lab := fakeRemote(t, "tok-lab", "build")

	out, err := loginHost(t, "tok-prod", "prod", prod.URL, "--label", "Production")
	if err != nil || !strings.Contains(out, "signed in to prod (Production) at "+prod.URL+" as ci, scope read") {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err = loginHost(t, "tok-lab", "lab", lab.URL, "--json"); err != nil || !strings.Contains(out, `"label": "lab"`) {
		t.Fatalf("login json: %v\n%s", err, out)
	}
	if out, err = loginHost(t, "nope", "bad", lab.URL); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Fatalf("wrong token: %v\n%s", err, out)
	}
	if out, err = loginHost(t, "", "empty", lab.URL); err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("empty token: %v\n%s", err, out)
	}

	wantCLI(t, []string{"host", "list"}, "ALIAS", "local", "(this machine)", "lab", "prod", "Production", prod.URL)
	if out := mustCLI(t, "host", "list", "--json"); strings.Contains(out, "tok-") {
		t.Fatalf("host list shows a token:\n%s", out)
	}

	out = wantCLI(t, []string{"workflow", "list", "--all-hosts"}, "REF", "lab:build", "prod:deploy", "Production")
	if strings.Index(out, "lab:build") > strings.Index(out, "prod:deploy") {
		t.Fatalf("hosts out of order:\n%s", out)
	}
	wantCLI(t, []string{"workflow", "list", "--host", "prod", "--json"}, `"ref": "prod:deploy"`, `"host_label": "Production"`)
	failCLI(t, "is signed in (HTTP 404)", "workflow", "list", "--host", "nope")

	// A host that stops answering is reported and fails the command, but
	// the others are still listed.
	lab.Close()
	out, err = runCLI(t, "", "workflow", "list", "--all-hosts")
	if err == nil || !strings.Contains(err.Error(), "1 of 3 host(s) could not be listed") || !strings.Contains(out, "prod:deploy") || !strings.Contains(out, "lab (lab):") {
		t.Fatalf("partial listing: %v\n%s", err, out)
	}

	wantCLI(t, []string{"host", "logout", "prod"}, "signed out of prod")
	failCLI(t, "is signed in (HTTP 404)", "host", "logout", "prod")
}

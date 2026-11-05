package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"icaro/internal/config"
)

// With daemon.ui_trust_loopback the UI listener serves the browser as the
// local user, and the hosts routes are wired (ADR 0033).
func TestTrustedLoopbackUIAndHosts(t *testing.T) {
	for _, trust := range []bool{false, true} {
		a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), func(c *config.Config) { c.Daemon.UITrustLoopback = trust })
		var ui *uiListener
		for _, c := range a.components {
			if u, ok := c.(*uiListener); ok {
				ui = u
			}
		}
		if ui == nil {
			t.Fatal("ui listener not registered")
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ui.Run(ctx) }()
		base, err := ui.Open()
		if err != nil {
			t.Fatal(err)
		}
		get := func(path string) (int, string) {
			resp, err := http.Get(base + path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		code, body := get("/api/v1/hosts")
		if trust && (code != 200 || !strings.Contains(body, `"alias": "local"`)) {
			t.Errorf("trusted hosts = %d %s", code, body)
		}
		if !trust && code != 401 {
			t.Errorf("untrusted hosts = %d %s", code, body)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

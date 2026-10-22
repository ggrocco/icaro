package main

import (
	"strings"
	"testing"
)

const profile = `mode: atomic
icaro/internal/a/a.go:1.1,2.2 3 1
icaro/internal/a/a.go:3.1,4.2 1 0
icaro/internal/a/a.go:3.1,4.2 1 2
icaro/internal/b/b.go:1.1,2.2 1 0
icaro/internal/b/b.go:3.1,4.2 9 1
icaro/cmd/icaro/main.go:1.1,2.2 5 0
`

func TestParseProfile(t *testing.T) {
	cov, err := ParseProfile(strings.NewReader(profile))
	if err != nil {
		t.Fatal(err)
	}
	// The duplicated block counts once, covered because one listing hit it.
	if got := cov["icaro/internal/a"]; got != (Coverage{Covered: 4, Total: 4}) {
		t.Errorf("a = %+v", got)
	}
	if got := cov["icaro/internal/b"]; got != (Coverage{Covered: 9, Total: 10}) {
		t.Errorf("b = %+v", got)
	}
	for _, bad := range []string{"", "nope\n", "mode: set\nx.go:1.1 1\n", "mode: set\nnoslash.go:1.1,2.2 1 1\n", "mode: set\na/b.go:1.1,2.2 x 1\n"} {
		if _, err := ParseProfile(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseProfile(%q) succeeded", bad)
		}
	}
}

func TestParseThresholds(t *testing.T) {
	exc, err := ParseThresholds(strings.NewReader("# header\n\nicaro/internal/b 80% # edge adapter\n"))
	if err != nil {
		t.Fatal(err)
	}
	if exc["icaro/internal/b"] != (Exception{Min: 80, Reason: "edge adapter"}) {
		t.Errorf("exc = %+v", exc)
	}
	for _, bad := range []string{
		"icaro/internal/b 80\n",                                    // no reason
		"icaro/internal/b 80 #\n",                                  // empty reason
		"icaro/internal/b # why\n",                                 // no minimum
		"icaro/internal/b 120 # why\n",                             // out of range
		"icaro/internal/b x # why\n",                               // not a number
		"icaro/internal/b 80 # why\nicaro/internal/b 70 # again\n", // duplicate
	} {
		if _, err := ParseThresholds(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseThresholds(%q) succeeded", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	cov := map[string]Coverage{
		"icaro/internal/full":  {Covered: 96, Total: 100},
		"icaro/internal/low":   {Covered: 50, Total: 100},
		"icaro/internal/edge":  {Covered: 30, Total: 100},
		"icaro/internal/empty": {},
		"icaro/cmd/icaro":      {Covered: 0, Total: 10},
	}
	cases := []struct {
		name string
		exc  map[string]Exception
		ok   bool
		want []string
	}{
		{"below default", map[string]Exception{"icaro/internal/edge": {Min: 30, Reason: "r"}}, false, []string{"FAIL  50.0%", "low below 95.0%", "ok    30.0%", "(exception, floor 30.0%: r)"}},
		{"all pass", map[string]Exception{"icaro/internal/edge": {Min: 28, Reason: "r"}, "icaro/internal/low": {Min: 50, Reason: "r"}}, true, []string{"ok    96.0%"}},
		{"stale floor", map[string]Exception{"icaro/internal/edge": {Min: 20, Reason: "r"}, "icaro/internal/low": {Min: 50, Reason: "r"}}, false, []string{"floor 20.0% is stale: raise it to 30"}},
		{"exception no longer needed", map[string]Exception{"icaro/internal/full": {Min: 90, Reason: "r"}, "icaro/internal/edge": {Min: 30, Reason: "r"}, "icaro/internal/low": {Min: 50, Reason: "r"}}, false, []string{"full meets the 95% target: remove the exception"}},
		{"unknown package", map[string]Exception{"icaro/internal/gone": {Min: 10, Reason: "r"}, "icaro/internal/edge": {Min: 30, Reason: "r"}, "icaro/internal/low": {Min: 50, Reason: "r"}}, false, []string{"exception for icaro/internal/gone, which is not in the profile"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report, ok := Check(cov, c.exc, 95, "icaro/internal/")
			if ok != c.ok {
				t.Errorf("ok = %v\n%s", ok, report)
			}
			for _, w := range c.want {
				if !strings.Contains(report, w) {
					t.Errorf("report lacks %q:\n%s", w, report)
				}
			}
			if strings.Contains(report, "cmd/icaro") {
				t.Errorf("package outside the prefix reported:\n%s", report)
			}
			if !strings.Contains(report, "100.0%    0/0    icaro/internal/empty") {
				t.Errorf("package without statements should pass at 100%%:\n%s", report)
			}
		})
	}
}

package issue

import "testing"

func TestIssues(t *testing.T) {
	is := Issues{{Path: "/a", Message: "bad", Hint: "fix"}, {Path: "/b", Message: "missing"}}
	if got := is.Error(); got != "/a: bad (hint: fix); /b: missing" {
		t.Fatalf("%q", got)
	}
}

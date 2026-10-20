package engine

import (
	"archive/tar"
	"testing"
)

// Regression: step I/O files must belong to the step user, or a non-root
// step (a root daemon runs steps as 1000:1000) cannot write output.json.
func TestTarballOwnership(t *testing.T) {
	uid, gid := parseUser("1000:1001")
	if uid != 1000 || gid != 1001 {
		t.Fatalf("parseUser = %d:%d", uid, gid)
	}
	if u, g := parseUser("42"); u != 42 || g != 42 {
		t.Fatalf("uid only = %d:%d", u, g)
	}
	if u, g := parseUser(""); u != 0 || g != 0 {
		t.Fatalf("empty = %d:%d", u, g)
	}
	tr := tar.NewReader(tarball([]tarFile{{Name: "output.json", Data: []byte("{}"), UID: uid, GID: gid}}))
	h, err := tr.Next()
	if err != nil || h.Uid != 1000 || h.Gid != 1001 {
		t.Fatalf("header %+v %v", h, err)
	}
}

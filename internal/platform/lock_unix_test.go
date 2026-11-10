//go:build unix

package platform

import "testing"

func TestDetachStartsASession(t *testing.T) {
	if a := Detach(); !a.Setsid {
		t.Fatalf("Detach = %+v", a)
	}
}

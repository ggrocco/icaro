package logs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestScrubber(t *testing.T) {
	var out bytes.Buffer
	s := NewScrubber(&out, []string{"xoxb-secret-token", "short", "secret-token"})
	_, _ = s.Write([]byte("token is xoxb-sec"))
	_, _ = s.Write([]byte("ret-token here\nshort stays\nsecret-token alone"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "token is *** here\nshort stays\n*** alone"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}

func TestStoreAndTail(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	f, err := st.Open("run1", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		_, _ = f.WriteString("line number " + string(rune('A'+i%26)) + "\n")
	}
	_ = f.Close()
	if st.Path("run1", 0) != filepath.Join(st.Dir, "run1", "0.log") {
		t.Fatal(st.Path("run1", 0))
	}
	tail, err := Tail(st.Path("run1", 0), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > 50 || !bytes.HasPrefix(tail, []byte("line number")) || !bytes.HasSuffix(tail, []byte("\n")) {
		t.Fatalf("tail %q", tail)
	}
	all, _ := Tail(st.Path("run1", 0), 1<<20)
	if info, _ := os.Stat(st.Path("run1", 0)); int64(len(all)) != info.Size() {
		t.Fatal("full tail should return everything")
	}
}

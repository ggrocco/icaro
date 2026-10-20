package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shipped examples must stay valid.
func TestExamplesAreValid(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, issues, err := Parse(src, Options{MaxTimeout: time.Hour}); err != nil || len(issues) > 0 {
			t.Errorf("%s: %v %v", f, err, issues)
		}
	}
}

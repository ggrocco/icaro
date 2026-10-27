// Package docscheck keeps the documented workflows honest: every workflow
// in examples/ and every docs code block fenced as ```yaml workflow must
// validate and pin each image by digest (Phase 10: "examples use images
// pinned by digest").
package docscheck

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"icaro/internal/integration"
	"icaro/internal/workflow"
)

var (
	imageLine = regexp.MustCompile(`^\s*image:\s*["']?([^"'\s#]+)`)
	pinned    = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

type doc struct {
	where string
	src   string
}

func examples(t *testing.T, root string) []doc {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "examples", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	var out []doc
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, doc{where: f, src: string(b)})
	}
	return out
}

// workflowBlocks returns ```yaml workflow fences from the markdown files.
func workflowBlocks(t *testing.T, root string) []doc {
	t.Helper()
	var files []string
	for _, pattern := range []string{"docs/*.md", "README.md"} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	var out []doc
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		var cur *strings.Builder
		start, n := 0, 0
		for sc.Scan() {
			n++
			line := sc.Text()
			switch {
			case cur == nil && strings.TrimSpace(line) == "```yaml workflow":
				cur, start = &strings.Builder{}, n
			case cur != nil && strings.TrimSpace(line) == "```":
				out = append(out, doc{where: f + ":" + strconv.Itoa(start), src: cur.String()})
				cur = nil
			case cur != nil:
				cur.WriteString(line + "\n")
			}
		}
		_ = fh.Close()
	}
	return out
}

func TestDocumentedWorkflowsValidateAndPinImages(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	docs := append(examples(t, root), workflowBlocks(t, root)...)
	blocks := 0
	// Integration triggers and uses: steps are checked against the
	// integrations shipped in the binary.
	opts := workflow.Options{Catalog: integration.Catalog{R: integration.NewRegistry(t.TempDir(), nil)}}
	for _, d := range docs {
		if strings.Contains(d.where, ".md:") {
			blocks++
		}
		_, issues, err := workflow.Parse([]byte(d.src), opts)
		if err != nil {
			t.Errorf("%s: %v", d.where, err)
			continue
		}
		for _, is := range issues {
			t.Errorf("%s: %s", d.where, is.String())
		}
		for _, line := range strings.Split(d.src, "\n") {
			m := imageLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s: image %q is not pinned by digest (use name:tag@sha256:<digest>)", d.where, m[1])
			}
		}
	}
	if blocks == 0 {
		t.Fatal("no ```yaml workflow blocks found in docs; the check would be vacuous")
	}
}

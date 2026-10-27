package skills

import (
	"bufio"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/integration"
	"icaro/internal/workflow"
)

// A block is a workflow when it has a top-level version or steps key;
// other yaml blocks are fragments (an env/command snippet).
var workflowKey = regexp.MustCompile(`(?m)^(version|steps):`)

type block struct {
	where string
	src   string
}

// yamlBlocks returns every ```yaml fence of every markdown file in FS.
func yamlBlocks(t *testing.T) []block {
	t.Helper()
	var out []block
	err := fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		f, err := FS.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		var cur *strings.Builder
		start, n := 0, 0
		for sc.Scan() {
			n++
			line := sc.Text()
			trimmed := strings.TrimSpace(line)
			switch {
			case cur == nil && strings.HasPrefix(trimmed, "```yaml"):
				cur, start = &strings.Builder{}, n
			case cur != nil && trimmed == "```":
				out = append(out, block{where: path + ":" + strconv.Itoa(start), src: cur.String()})
				cur = nil
			case cur != nil:
				cur.WriteString(line + "\n")
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The skills teach agents to write workflows; their examples must pass
// the validator against the integrations shipped in the binary, or the
// agent learns field names that do not exist.
func TestSkillWorkflowsValidateAgainstBuiltinCatalog(t *testing.T) {
	reg := integration.NewRegistry(t.TempDir(), nil)
	if errs := reg.Snapshot().Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	opts := workflow.Options{MaxTimeout: 30 * time.Minute, Catalog: integration.Catalog{R: reg}}
	checked := 0
	for _, b := range yamlBlocks(t) {
		if !workflowKey.MatchString(b.src) {
			continue
		}
		checked++
		_, issues, err := workflow.Parse([]byte(b.src), opts)
		if err != nil {
			t.Errorf("%s: %v", b.where, err)
			continue
		}
		for _, is := range issues {
			t.Errorf("%s: %s", b.where, is)
		}
	}
	if checked == 0 {
		t.Fatal("no workflow examples found in the skills; the check would be vacuous")
	}
}

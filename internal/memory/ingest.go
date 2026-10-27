package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"icaro/internal/store"
)

// IngestOptions bound deterministic ingest.
type IngestOptions struct {
	MaxFiles    int   // default 20000
	MaxFileSize int64 // default 1 MiB
	Commits     int   // recent commits to ingest; default 200, <0 disables
}

// IngestReport summarizes an ingest pass.
type IngestReport struct {
	Root      string `json:"root"`
	Files     int    `json:"files"`
	Symbols   int    `json:"symbols"`
	ADRs      int    `json:"adrs"`
	Commits   int    `json:"commits"`
	Unchanged int    `json:"unchanged"`
	Removed   int    `json:"removed"`
	Skipped   int    `json:"skipped"`
	// Excluded counts files left out because they may hold secrets: dotfiles,
	// key material and files git ignores.
	Excluded int `json:"excluded"`
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, "target": true,
	".venv": true, "venv": true, "__pycache__": true, ".idea": true, ".vscode": true, ".claude": true,
	".next": true, ".cache": true, "coverage": true, "bin": true,
}

var languages = map[string]string{
	".go": "go", ".ts": "typescript", ".tsx": "typescript", ".js": "javascript", ".jsx": "javascript", ".mjs": "javascript",
	".py": "python", ".md": "markdown", ".yaml": "yaml", ".yml": "yaml", ".json": "json", ".sql": "sql", ".sh": "shell",
	".rs": "rust", ".java": "java", ".rb": "ruby", ".css": "css", ".html": "html", ".toml": "toml",
}

// ingestFormat versions what ingest stores of a file. It is part of the
// content hash, so bumping it (new redaction rules) re-indexes unchanged
// files once instead of keeping what an older ingest stored.
const ingestFormat = "2"

func ingestHash(content []byte) string {
	sum := sha256.Sum256(append([]byte(ingestFormat+"\x00"), content...))
	return hex.EncodeToString(sum[:])
}

// stableID makes ingest idempotent: the same fact always gets the same id.
func stableID(project, kind, key string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + kind + "\x00" + key))
	return "m" + hex.EncodeToString(sum[:12])
}

// Ingest indexes files, symbols, imports, ADRs and recent commits under
// root without agent involvement. Re-ingesting unchanged content is a
// no-op; files that disappeared are removed from memory.
func (e *Engine) Ingest(ctx context.Context, scope Scope, root string, opts IngestOptions) (*IngestReport, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 20000
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 1 << 20
	}
	if opts.Commits == 0 {
		opts.Commits = 200
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalid, root)
	}
	// git reports symlink-resolved paths (macOS: /var -> /private/var), so
	// resolve ours too or a subtree looks like it is outside the repo and
	// is ingested as a whole project, dropping every other file's facts.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	base := GitTopLevel(abs)
	prefix, _ := filepath.Rel(base, abs)
	prefix = filepath.ToSlash(prefix)
	if prefix == "." || strings.HasPrefix(prefix, "..") {
		prefix = ""
		base = abs
	}

	existing, err := e.db.ListMemoryKeys(ctx, scope.ProjectID, OriginIngest)
	if err != nil {
		return nil, err
	}
	hashByID := map[string]string{}
	idsBySource := map[string][]string{}
	for _, k := range existing {
		hashByID[k.ID] = k.ContentHash
		if k.Kind != KindCommit {
			idsBySource[k.Source] = append(idsBySource[k.Source], k.ID)
		}
	}

	rep := &IngestReport{Root: abs}
	seen := map[string]bool{}
	var pending []store.MemoryBundle
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := e.db.PutMemories(ctx, pending)
		pending = pending[:0]
		return err
	}
	now := e.now()
	newRow := func(kind, key, title, body, source string, line int, hash string, meta map[string]any) *store.MemoryRow {
		m, _ := json.Marshal(meta)
		return &store.MemoryRow{
			ID: stableID(scope.ProjectID, kind, key), ProjectID: scope.ProjectID, Origin: OriginIngest, Kind: kind,
			Title: title, Body: body, Source: source, SourceLine: line, ContentHash: hash, Metadata: string(m),
			CreatedAt: now, UpdatedAt: now,
		}
	}

	// Reads go through an os.Root so a symlink swapped in during the walk
	// cannot escape the ingested tree.
	fsRoot, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fsRoot.Close() }()
	listed, listedDirs, inGit := gitListed(ctx, abs)
	files := 0
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inRoot, _ := filepath.Rel(abs, p)
		inRoot = filepath.ToSlash(inRoot)
		if d.IsDir() {
			if p != abs && (skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".github")) {
				return fs.SkipDir
			}
			if p != abs && inGit && !listedDirs[inRoot] {
				return fs.SkipDir // ignored by git, or empty
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if sensitiveFile(d.Name()) || (inGit && !listed[inRoot]) {
			rep.Excluded++
			return nil
		}
		if files >= opts.MaxFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || info.Size() > opts.MaxFileSize {
			rep.Skipped++
			return nil
		}
		content, err := fsRoot.ReadFile(filepath.FromSlash(inRoot))
		if err != nil || isBinary(content) {
			rep.Skipped++
			return nil
		}
		files++
		relp, _ := filepath.Rel(base, p)
		rel := filepath.ToSlash(relp)
		hash := ingestHash(content)
		fileID := stableID(scope.ProjectID, KindFile, rel)
		if hashByID[fileID] == hash {
			rep.Unchanged++
			for _, id := range idsBySource[rel] {
				seen[id] = true
			}
			return nil
		}
		lang := languages[strings.ToLower(filepath.Ext(p))]
		facts := extract(rel, lang, content)
		fileRow := newRow(KindFile, rel, rel, redactText(snippet(content, 2048), lang), rel, 0, hash,
			map[string]any{"size": info.Size(), "language": lang, "lines": bytes.Count(content, []byte("\n")) + 1})
		fileRow.Link = "file://" + filepath.ToSlash(p)
		fb := bundle(fileRow)
		for _, imp := range facts.imports {
			fb.Edges = append(fb.Edges, store.MemoryEdge{ToRef: "import:" + imp, Kind: "imports"})
		}
		var symBundles []store.MemoryBundle
		for _, s := range facts.symbols {
			row := newRow(KindSymbol, rel+"#"+s.name, s.name, redactText(s.doc, lang), rel, s.line, hash, map[string]any{"language": lang, "kind": s.kind})
			row.Link = fmt.Sprintf("file://%s#L%d", filepath.ToSlash(p), s.line)
			sb := bundle(row)
			sb.Edges = append(sb.Edges, store.MemoryEdge{ToRef: fileRow.ID, Kind: "defined_in"})
			fb.Edges = append(fb.Edges, store.MemoryEdge{ToRef: row.ID, Kind: "defines"})
			symBundles = append(symBundles, sb)
			seen[row.ID] = true
			rep.Symbols++
		}
		seen[fileRow.ID] = true
		pending = append(append(pending, fb), symBundles...)
		rep.Files++
		if adr, ok := parseADR(rel, content); ok {
			row := newRow(KindADR, rel, adr.title, redactText(snippet(content, 16<<10), "markdown"), rel, 1, hash, map[string]any{"status": adr.status})
			row.Link = fileRow.Link
			ab := bundle(row)
			ab.Edges = append(ab.Edges, store.MemoryEdge{ToRef: fileRow.ID, Kind: "defined_in"})
			pending = append(pending, ab)
			seen[row.ID] = true
			rep.ADRs++
		}
		if len(pending) >= 200 {
			return flush()
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := flush(); err != nil {
		return nil, err
	}

	if opts.Commits > 0 {
		commits, err := gitCommits(ctx, abs, opts.Commits)
		if err == nil {
			for _, c := range commits {
				id := stableID(scope.ProjectID, KindCommit, c.hash)
				if _, ok := hashByID[id]; ok {
					continue
				}
				row := newRow(KindCommit, c.hash, redactText(c.subject, ""), redactText(strings.TrimSpace(c.body+"\n\nAuthor: "+c.author), ""), "commit:"+c.hash, 0, c.hash,
					map[string]any{"author": c.author, "date": c.date.Format(time.RFC3339), "files": c.files})
				row.Link = "commit:" + c.hash
				b := bundle(row)
				for _, f := range c.files {
					b.Edges = append(b.Edges, store.MemoryEdge{ToRef: stableID(scope.ProjectID, KindFile, f), Kind: "touches"})
				}
				pending = append(pending, b)
				rep.Commits++
				if len(pending) >= 200 {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}

	// Remove facts for files that disappeared under the ingested subtree.
	var gone []string
	for _, k := range existing {
		if k.Kind == KindCommit || seen[k.ID] {
			continue
		}
		if prefix != "" && k.Source != prefix && !strings.HasPrefix(k.Source, prefix+"/") {
			continue
		}
		gone = append(gone, k.ID)
	}
	if err := e.db.DeleteMemories(ctx, scope.ProjectID, gone); err != nil {
		return nil, err
	}
	rep.Removed = len(gone)
	return rep, nil
}

func isBinary(b []byte) bool {
	n := min(len(b), 8000)
	return bytes.IndexByte(b[:n], 0) >= 0
}

func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	cut := max
	for cut > 0 && b[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(b[:cut])
}

type symbol struct {
	name, kind, doc string
	line            int
}

type facts struct {
	symbols []symbol
	imports []string
}

func extract(rel, lang string, content []byte) facts {
	switch lang {
	case "go":
		if f, ok := extractGo(rel, content); ok {
			return f
		}
	case "typescript", "javascript":
		return extractRegex(content, jsSymbols, jsImports)
	case "python":
		return extractRegex(content, pySymbols, pyImports)
	}
	return facts{}
}

func extractGo(rel string, content []byte) (facts, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return facts{}, false
	}
	var out facts
	for _, imp := range f.Imports {
		out.imports = append(out.imports, strings.Trim(imp.Path.Value, `"`))
	}
	pkg := f.Name.Name
	src := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(content) || a > b {
			return ""
		}
		return string(content[a:b])
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := pkg + "." + d.Name.Name
			kind := "func"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = pkg + "." + recvName(d.Recv.List[0].Type) + "." + d.Name.Name
				kind = "method"
			}
			doc := strings.TrimSpace(d.Doc.Text() + "\n" + src(d.Pos(), d.Type.End()))
			out.symbols = append(out.symbols, symbol{name: name, kind: kind, doc: doc, line: fset.Position(d.Pos()).Line})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					doc := s.Doc.Text()
					if doc == "" {
						doc = d.Doc.Text()
					}
					out.symbols = append(out.symbols, symbol{name: pkg + "." + s.Name.Name, kind: "type", doc: strings.TrimSpace(doc + "\ntype " + s.Name.Name), line: fset.Position(s.Pos()).Line})
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if !n.IsExported() {
							continue
						}
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						out.symbols = append(out.symbols, symbol{name: pkg + "." + n.Name, kind: kind, doc: strings.TrimSpace(s.Doc.Text() + d.Doc.Text()), line: fset.Position(n.Pos()).Line})
					}
				}
			}
		}
	}
	return out, true
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return "?"
}

type symbolRe struct {
	re   *regexp.Regexp
	kind string
}

var (
	jsSymbols = []symbolRe{
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), "function"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
		{regexp.MustCompile(`^\s*export\s+(?:declare\s+)?(?:const|let|var|interface|type|enum)\s+([A-Za-z_$][\w$]*)`), "export"},
	}
	jsImports = []*regexp.Regexp{
		regexp.MustCompile(`^\s*import\s+(?:[^'"]*?\s+from\s+)?['"]([^'"]+)['"]`),
		regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`),
	}
	pySymbols = []symbolRe{
		{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`), "function"},
		{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`), "class"},
	}
	pyImports = []*regexp.Regexp{
		regexp.MustCompile(`^\s*import\s+([\w.]+)`),
		regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\b`),
	}
)

// extractRegex is the fallback symbol extractor for languages without a
// parser in the standard library. It is line-based and deliberately simple.
func extractRegex(content []byte, syms []symbolRe, imps []*regexp.Regexp) facts {
	var out facts
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	seenImp := map[string]bool{}
	for sc.Scan() {
		line++
		text := sc.Text()
		for _, s := range syms {
			if m := s.re.FindStringSubmatch(text); m != nil {
				out.symbols = append(out.symbols, symbol{name: m[1], kind: s.kind, doc: strings.TrimSpace(text), line: line})
				break
			}
		}
		for _, re := range imps {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				if !seenImp[m[1]] {
					seenImp[m[1]] = true
					out.imports = append(out.imports, m[1])
				}
			}
		}
	}
	return out
}

type adrInfo struct{ title, status string }

var (
	adrName   = regexp.MustCompile(`^\d{3,4}[-_].+\.md$`)
	adrStatus = regexp.MustCompile(`(?im)^\s*(?:\*\*status:?\*\*:?|status:)\s*(.+)$`)
)

// parseADR recognizes architecture decision records: markdown files in an
// adr/ or decisions/ directory, or named NNNN-title.md.
func parseADR(rel string, content []byte) (adrInfo, bool) {
	if strings.ToLower(path.Ext(rel)) != ".md" {
		return adrInfo{}, false
	}
	dir := strings.ToLower(path.Base(path.Dir(rel)))
	if dir != "adr" && dir != "adrs" && dir != "decisions" && !adrName.MatchString(path.Base(rel)) {
		return adrInfo{}, false
	}
	info := adrInfo{title: strings.TrimSuffix(path.Base(rel), ".md")}
	for _, l := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(l, "# ") {
			info.title = strings.TrimSpace(l[2:])
			break
		}
	}
	if m := adrStatus.FindStringSubmatch(string(content)); m != nil {
		info.status = strings.TrimSpace(strings.Trim(m[1], "* "))
	}
	return info, true
}

type commit struct {
	hash, author, subject, body string
	date                        time.Time
	files                       []string
}

// gitCommits reads the most recent commits of the repository at dir.
func gitCommits(ctx context.Context, dir string, n int) ([]commit, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := git(ctx, dir, "log", "-n", fmt.Sprint(n), "--no-color",
		"--format=%x1e%H%x1f%an%x1f%aI%x1f%s%x1f%b%x1f", "--name-only")
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 6)
		if len(parts) < 6 {
			continue
		}
		c := commit{hash: parts[0], author: parts[1], subject: parts[3], body: strings.TrimSpace(parts[4])}
		c.date, _ = time.Parse(time.RFC3339, parts[2])
		for _, f := range strings.Split(parts[5], "\n") {
			if f = strings.TrimSpace(f); f != "" {
				c.files = append(c.files, f)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

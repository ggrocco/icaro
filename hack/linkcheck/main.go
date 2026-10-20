// Command linkcheck verifies relative links in the repository's Markdown
// and HTML files offline: the target file must exist and, for a #fragment
// into Markdown, the heading must exist (GitHub slug rules). External
// URLs are not fetched.
//
//	go run ./hack/linkcheck [root]
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	problems, err := check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "linkcheck:", err)
		os.Exit(2)
	}
	report(os.Stdout, problems)
	if len(problems) > 0 {
		os.Exit(1)
	}
}

func report(w io.Writer, problems []string) {
	for _, p := range problems {
		fmt.Fprintln(w, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "%d broken link(s)\n", len(problems))
	}
}

var skipDirs = map[string]bool{".git": true, ".claude": true, "node_modules": true, "dist": true, "build": true, "testdata": true, "vendor": true}

var (
	mdLink   = regexp.MustCompile(`!?\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	mdRef    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+"[^"]*")?\s*$`)
	htmlLink = regexp.MustCompile(`(?i)(?:href|src)\s*=\s*"([^"]+)"`)
	codeSpan = regexp.MustCompile("`[^`]*`")
	heading  = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	htmlID   = regexp.MustCompile(`(?i)\b(?:id|name)\s*=\s*"([^"]+)"`)
)

func check(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(p)); ext == ".md" || ext == ".html" {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	anchors := map[string]map[string]bool{}
	var problems []string
	for _, f := range files {
		links, err := extract(f)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if msg := resolve(f, l, anchors); msg != "" {
				problems = append(problems, fmt.Sprintf("%s: %s: %s", f, l, msg))
			}
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// extract returns link targets, skipping fenced code blocks and code spans.
func extract(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	isHTML := strings.EqualFold(filepath.Ext(path), ".html")
	var out []string
	inFence := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !isHTML && strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if isHTML {
			for _, m := range htmlLink.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
			continue
		}
		line = codeSpan.ReplaceAllString(line, "")
		for _, re := range []*regexp.Regexp{mdLink, mdRef, htmlLink} {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				out = append(out, m[1])
			}
		}
	}
	return out, sc.Err()
}

func resolve(from, link string, cache map[string]map[string]bool) string {
	if link == "" || strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") || strings.HasPrefix(link, "data:") || strings.HasPrefix(link, "//") {
		return ""
	}
	target, frag, _ := strings.Cut(link, "#")
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	path := from
	if target != "" {
		if strings.HasPrefix(target, "/") {
			return "absolute paths break on GitHub and Pages; use a relative link"
		}
		path = filepath.Join(filepath.Dir(from), filepath.FromSlash(target))
		fi, err := os.Stat(path)
		if err != nil {
			return "target does not exist"
		}
		if fi.IsDir() {
			return ""
		}
	}
	if frag == "" {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".md" && ext != ".html" {
		return ""
	}
	a, ok := cache[path]
	if !ok {
		var err error
		a, err = anchorsOf(path)
		if err != nil {
			return err.Error()
		}
		cache[path] = a
	}
	if !a[strings.ToLower(frag)] {
		return fmt.Sprintf("no heading or id %q in %s", frag, path)
	}
	return ""
}

// anchorsOf collects GitHub heading slugs (with -1, -2 suffixes for
// duplicates) and explicit id/name attributes.
func anchorsOf(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	counts := map[string]int{}
	inFence := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		for _, m := range htmlID.FindAllStringSubmatch(line, -1) {
			out[strings.ToLower(m[1])] = true
		}
		if inFence {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			s := Slug(m[1])
			if n := counts[s]; n > 0 {
				out[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				out[s] = true
			}
			counts[s]++
		}
	}
	return out, nil
}

// Slug mirrors GitHub's heading anchors: lowercase, drop punctuation other
// than '-' and '_', spaces become '-'. Markdown links and code ticks in the
// heading contribute their text.
func Slug(h string) string {
	h = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(h, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

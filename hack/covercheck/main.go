// Command covercheck enforces the per-package coverage target (ADR 0025):
// every package under -prefix must reach -default percent of statements,
// unless the thresholds file lists it with a lower floor and a reason.
//
//	go test -coverprofile=coverage.out ./...
//	go run ./hack/covercheck -profile coverage.out
//
// The thresholds file has one package per line: import path, minimum
// percent, and a mandatory reason after "#". Blank lines and lines that
// start with "#" are ignored.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

func main() {
	profile := flag.String("profile", "coverage.out", "coverage profile from go test -coverprofile")
	thresholds := flag.String("thresholds", "hack/covercheck/thresholds.txt", "per-package exceptions")
	def := flag.Float64("default", 95, "minimum percent for packages without an exception")
	prefix := flag.String("prefix", "icaro/internal/", "only packages under this import path are checked")
	flag.Parse()

	pf, err := os.Open(*profile)
	if err != nil {
		fail(err)
	}
	defer func() { _ = pf.Close() }()
	cov, err := ParseProfile(pf)
	if err != nil {
		fail(err)
	}
	tf, err := os.Open(*thresholds)
	if err != nil {
		fail(err)
	}
	defer func() { _ = tf.Close() }()
	exc, err := ParseThresholds(tf)
	if err != nil {
		fail(err)
	}
	report, ok := Check(cov, exc, *def, *prefix)
	fmt.Print(report)
	if !ok {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "covercheck:", err)
	os.Exit(2)
}

// Coverage is the statement count of one package.
type Coverage struct{ Covered, Total int }

// Percent is 100 for a package without statements.
func (c Coverage) Percent() float64 {
	if c.Total == 0 {
		return 100
	}
	return 100 * float64(c.Covered) / float64(c.Total)
}

// Exception is a package allowed below the default target.
type Exception struct {
	Min    float64
	Reason string
}

// ParseProfile sums statements per package. A block listed more than once
// (several test binaries covering one package) counts as covered if any
// listing hit it.
func ParseProfile(r io.Reader) (map[string]Coverage, error) {
	type block struct {
		pkg     string
		stmts   int
		covered bool
	}
	blocks := map[string]*block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, errors.New("not a coverage profile: missing mode line")
			}
			continue
		}
		if line == "" {
			continue
		}
		// file.go:l.c,l.c stmts count
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		stmts, err1 := strconv.Atoi(f[1])
		count, err2 := strconv.Atoi(f[2])
		colon := strings.LastIndex(f[0], ":")
		slash := strings.LastIndex(f[0][:max(colon, 0)], "/")
		if err1 != nil || err2 != nil || colon < 0 || slash < 0 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		b := blocks[f[0]]
		if b == nil {
			b = &block{pkg: f[0][:slash], stmts: stmts}
			blocks[f[0]] = b
		}
		b.covered = b.covered || count > 0
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, errors.New("empty coverage profile")
	}
	out := map[string]Coverage{}
	for _, b := range blocks {
		c := out[b.pkg]
		c.Total += b.stmts
		if b.covered {
			c.Covered += b.stmts
		}
		out[b.pkg] = c
	}
	return out, nil
}

// ParseThresholds reads the exceptions file. Every entry needs a reason.
func ParseThresholds(r io.Reader) (map[string]Exception, error) {
	out := map[string]Exception{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spec, reason, _ := strings.Cut(line, "#")
		reason = strings.TrimSpace(reason)
		f := strings.Fields(spec)
		if len(f) != 2 {
			return nil, fmt.Errorf("line %d: want \"<package> <min%%> # <reason>\"", n)
		}
		min, err := strconv.ParseFloat(strings.TrimSuffix(f[1], "%"), 64)
		if err != nil || min < 0 || min > 100 {
			return nil, fmt.Errorf("line %d: bad minimum %q", n, f[1])
		}
		if reason == "" {
			return nil, fmt.Errorf("line %d: %s needs a reason after #", n, f[0])
		}
		if _, dup := out[f[0]]; dup {
			return nil, fmt.Errorf("line %d: %s listed twice", n, f[0])
		}
		out[f[0]] = Exception{Min: min, Reason: reason}
	}
	return out, sc.Err()
}

// slack is how far above its floor an exception may sit before the report
// asks for the floor to be raised, so floors track real coverage.
const slack = 5

// Check reports every package under prefix and whether all meet their
// minimum. An exception for a package that is no longer in the profile is
// an error too, so the file cannot go stale.
func Check(cov map[string]Coverage, exc map[string]Exception, def float64, prefix string) (string, bool) {
	var pkgs []string
	for p := range cov {
		if strings.HasPrefix(p, prefix) {
			pkgs = append(pkgs, p)
		}
	}
	sort.Strings(pkgs)
	var b strings.Builder
	ok := true
	for _, p := range pkgs {
		c := cov[p]
		pct := c.Percent()
		e, isExc := exc[p]
		min := def
		if isExc {
			min = e.Min
		}
		status := "ok  "
		note := ""
		switch {
		case pct < min:
			status, ok = "FAIL", false
			note = fmt.Sprintf(" below %.1f%%", min)
		case isExc && pct >= def:
			status, ok = "FAIL", false
			note = fmt.Sprintf(" meets the %.0f%% target: remove the exception", def)
		case isExc && pct >= min+slack:
			status, ok = "FAIL", false
			note = fmt.Sprintf(" exception floor %.1f%% is stale: raise it to %.0f", min, pct)
		case isExc:
			note = fmt.Sprintf(" (exception, floor %.1f%%: %s)", min, e.Reason)
		}
		fmt.Fprintf(&b, "%s %5.1f%% %4d/%-4d %s%s\n", status, pct, c.Covered, c.Total, p, note)
	}
	var stale []string
	for p := range exc {
		if _, seen := cov[p]; !seen {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		ok = false
		fmt.Fprintf(&b, "FAIL exception for %s, which is not in the profile\n", p)
	}
	return b.String(), ok
}

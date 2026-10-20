package memory

import (
	"strings"
	"unicode"
)

// maxTermLen caps indexed term length; longer runs are usually hashes.
const maxTermLen = 48

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "for": true,
	"from": true, "in": true, "is": true, "it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "was": true, "with": true, "we": true, "will": true, "not": true, "but": true,
}

// Tokenize splits text into normalized terms: lowercase runs of letters
// and digits, with camelCase and snake_case identifiers also split into
// their parts ("ParseTemplate" -> parsetemplate, parse, template), light
// plural stemming, and stopwords dropped.
func Tokenize(s string) []string {
	var out []string
	emit := func(t string) {
		t = stem(strings.ToLower(t))
		if len([]rune(t)) < 2 || len(t) > maxTermLen || stopwords[t] {
			return
		}
		out = append(out, t)
	}
	for _, word := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		parts := splitIdent(word)
		if len(parts) > 1 {
			emit(strings.ReplaceAll(word, "_", ""))
		}
		for _, p := range parts {
			emit(p)
		}
	}
	return out
}

// splitIdent splits snake_case and camelCase words.
func splitIdent(w string) []string {
	var parts []string
	for _, seg := range strings.Split(w, "_") {
		if seg == "" {
			continue
		}
		rs := []rune(seg)
		start := 0
		for i := 1; i < len(rs); i++ {
			lowerToUpper := unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i])
			acronymEnd := i+1 < len(rs) && unicode.IsUpper(rs[i-1]) && unicode.IsUpper(rs[i]) && unicode.IsLower(rs[i+1])
			letterDigit := unicode.IsLetter(rs[i-1]) != unicode.IsLetter(rs[i])
			if lowerToUpper || acronymEnd || letterDigit {
				parts = append(parts, string(rs[start:i]))
				start = i
			}
		}
		parts = append(parts, string(rs[start:]))
	}
	return parts
}

// stem strips common English plural endings; enough for "decisions" to
// find "decision" without a stemming dependency.
func stem(t string) string {
	switch {
	case len(t) > 4 && strings.HasSuffix(t, "ies"):
		return t[:len(t)-3] + "y"
	case len(t) > 4 && (strings.HasSuffix(t, "ches") || strings.HasSuffix(t, "shes") || strings.HasSuffix(t, "xes") || strings.HasSuffix(t, "sses")):
		return t[:len(t)-2]
	case len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") && !strings.HasSuffix(t, "us") && !strings.HasSuffix(t, "is"):
		return t[:len(t)-1]
	}
	return t
}

// termCounts returns term frequencies.
func termCounts(terms []string) map[string]int {
	m := make(map[string]int, len(terms))
	for _, t := range terms {
		m[t]++
	}
	return m
}

// uniqueTerms returns the distinct terms of a query, capped.
func uniqueTerms(s string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range Tokenize(s) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
			if len(out) == max {
				break
			}
		}
	}
	return out
}

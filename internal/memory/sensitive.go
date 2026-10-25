package memory

import (
	"context"
	"path"
	"regexp"
	"strings"
	"time"

	"icaro/internal/logs"
)

// Ingest serves what it indexes to every agent over MCP, so it keeps
// secrets out (03-security): it skips dotfiles, files git ignores and
// files named like key material, and redacts credentials from what it
// stores of the rest.

// harmlessDotfiles are the dotfiles worth indexing: tool configuration
// that never holds credentials. Every other dotfile (.env, .npmrc,
// .netrc, …) is skipped.
var harmlessDotfiles = map[string]bool{
	".gitignore": true, ".gitattributes": true, ".dockerignore": true, ".editorconfig": true,
	".golangci.yml": true, ".golangci.yaml": true, ".goreleaser.yml": true, ".goreleaser.yaml": true,
	".pre-commit-config.yaml": true, ".tool-versions": true, ".nvmrc": true,
}

// keyMaterialExts are extensions of private keys and keystores.
var keyMaterialExts = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true, ".kdbx": true, ".ppk": true,
}

// dataExts are the extensions a credentials or secrets file comes in;
// credentials.go or secrets.ts is code and stays indexed.
var dataExts = map[string]bool{
	"": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".xml": true, ".txt": true,
	".env": true, ".conf": true, ".cfg": true, ".properties": true, ".csv": true,
}

// sensitiveFile reports a file name ingest never reads.
func sensitiveFile(name string) bool {
	l := strings.ToLower(name)
	if strings.HasPrefix(l, ".") {
		return !harmlessDotfiles[l]
	}
	ext := path.Ext(l)
	switch {
	case keyMaterialExts[ext]:
		return true
	case strings.HasPrefix(l, "id_") && (ext == "" || ext == ".pub"): // id_rsa, id_ed25519.pub
		return true
	case (strings.HasPrefix(l, "credentials") || strings.HasPrefix(l, "secrets.") || strings.HasPrefix(l, "secret.")) && dataExts[ext]:
		return true
	}
	return false
}

// gitListed returns the files git would track under dir (tracked, plus
// untracked ones that are not ignored), relative to dir, and every
// directory holding one. ok is false when dir is not in a git work tree;
// ingest then relies on its name rules and redaction alone.
func gitListed(ctx context.Context, dir string) (files, dirs map[string]bool, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, nil, false
	}
	files, dirs = map[string]bool{}, map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		files[f] = true
		for d := path.Dir(f); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	return files, dirs, true
}

// secretAssignment matches an environment-style assignment whose name
// says it holds a credential (STRIPE_SECRET_KEY=..., export GH_TOKEN=...).
var secretAssignment = regexp.MustCompile(`(?m)^(\s*(?:export\s+)?[A-Z][A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIALS?)[A-Z0-9_]*\s*[=:]\s*)\S.*$`)

// secretConfigKey matches a config entry (YAML, JSON, TOML, INI) whose key
// names a credential. It runs on configuration and text, not on code,
// where `token := next()` is not a secret.
var secretConfigKey = regexp.MustCompile(`(?im)^(\s*["']?[a-z0-9_.-]*(?:secret|token|password|passwd|api[_-]?key|private[_-]?key|access[_-]?key)["']?\s*[:=]\s*)["']?[^"'\s#,]{6,}["']?`)

// codeLanguages are languages whose files only get token-pattern
// redaction (see secretConfigKey).
var codeLanguages = map[string]bool{
	"go": true, "typescript": true, "javascript": true, "python": true, "rust": true, "java": true, "ruby": true,
	"css": true, "html": true, "shell": true, "sql": true,
}

// redactText removes credentials from text ingest stores: known token
// formats (the log redactor's patterns) everywhere, and the values of
// secret-named assignments and config keys outside code.
func redactText(s, lang string) string {
	s = logs.RedactString(s, nil)
	s = secretAssignment.ReplaceAllString(s, "${1}"+logs.Redacted)
	if !codeLanguages[lang] {
		s = secretConfigKey.ReplaceAllString(s, "${1}"+logs.Redacted)
	}
	return s
}

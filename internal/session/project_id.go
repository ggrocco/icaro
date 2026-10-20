package session

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// ProjectIDForRoot is the stable project id for a repository's main
// worktree root. Sessions, memory and the MCP bridge all derive scope
// from it, so `icaro mcp` on the host and an agent inside a session of
// the same repository share one memory scope (ADR 0004).
func ProjectIDForRoot(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return "p_" + hex.EncodeToString(sum[:8])
}

// Package skills embeds the agent skills shipped with Ícaro, versioned
// with the engine (architecture §10).
package skills

import "embed"

// FS holds one directory per skill (e.g. icaro-workflows/SKILL.md).
//
//go:embed icaro-workflows
var FS embed.FS

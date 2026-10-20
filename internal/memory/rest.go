package memory

// Request bodies of the REST fallback (/api/v1/memory), shared by the API
// server and its Go client.

// RESTWrite is the body of POST /api/v1/memory.
type RESTWrite struct {
	Project    string         `json:"project"`
	Branch     string         `json:"branch,omitempty"`
	Worktree   string         `json:"worktree,omitempty"`
	Agent      string         `json:"agent,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Origin     string         `json:"origin,omitempty"` // cli | api
	Kind       string         `json:"kind"`
	Title      string         `json:"title"`
	Body       string         `json:"body,omitempty"`
	Source     string         `json:"source,omitempty"`
	SourceLine int            `json:"source_line,omitempty"`
	TTLSeconds int64          `json:"ttl_seconds,omitempty"`
	Supersedes string         `json:"supersedes,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// RESTAccept is the body of POST /api/v1/memory/{id}/accept.
type RESTAccept struct {
	Project   string `json:"project"`
	Agent     string `json:"agent,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// RESTIngest is the body of POST /api/v1/memory/ingest. Project
// defaults to the id derived from Path.
type RESTIngest struct {
	Project string `json:"project,omitempty"`
	Path    string `json:"path"`
	Commits int    `json:"commits,omitempty"`
}

// RESTImport is the body of POST /api/v1/memory/import.
type RESTImport struct {
	Project string `json:"project"`
	Format  string `json:"format"`
	Data    string `json:"data"`
}

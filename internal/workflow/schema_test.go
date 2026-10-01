package workflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed schema must match the reflected one; run `go generate ./...`
// after changing spec types.
func TestSchemaFileIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", "schema", "workflow.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, SchemaJSON()) {
		t.Fatal("schema/workflow.schema.json is stale: run `go generate ./...`")
	}
}

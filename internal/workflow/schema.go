package workflow

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/invopop/jsonschema"
)

//go:generate go run ../../hack/genschema -out ../../schema/workflow.schema.json

// SchemaID is the $id of the published workflow schema.
const SchemaID = "https://icaro.dev/schema/workflow"

var (
	schemaOnce sync.Once
	schemaJSON []byte
)

// Schema reflects the workflow JSON Schema from the Go types.
func Schema() *jsonschema.Schema {
	r := &jsonschema.Reflector{
		BaseSchemaID:               "https://icaro.dev/schema",
		RequiredFromJSONSchemaTags: true,
		ExpandedStruct:             true,
	}
	s := r.Reflect(&Workflow{})
	s.ID = SchemaID
	s.Title = "Icaro workflow"
	s.Description = "A workflow is a named, triggerable sequence of steps; each step runs a container or an HTTP request."
	return s
}

// SchemaJSON returns the schema as indented JSON (cached).
func SchemaJSON() []byte {
	schemaOnce.Do(func() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(Schema()); err != nil {
			panic(err)
		}
		schemaJSON = buf.Bytes()
	})
	return schemaJSON
}

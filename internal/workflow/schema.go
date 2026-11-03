package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

//go:generate go run ../../hack/genschema -out ../../schema/workflow.schema.json

// SchemaID is the $id of the published workflow schema.
const SchemaID = "https://icaro.dev/schemas/workflow/v1.json"

// The schema is generated from the Spec types: property names, nesting
// and JSON types come from reflection over the structs; constraints and
// descriptions come from fieldAnnotations. A drift test keeps the output
// equal to blueprint/docs/schemas/workflow.schema.json.

// defSpec describes one generated definition.
type defSpec struct {
	name        string
	typ         reflect.Type
	required    []string
	description string
	// fields restricts the definition to a subset of the struct's fields
	// (union types such as TriggerSpec split into variants).
	fields []string
	// oneOfRequired adds oneOf: [{required:[a]}, {required:[b]}].
	oneOfRequired []string
}

var (
	typeSpec       = reflect.TypeOf(Spec{})
	typeTrigger    = reflect.TypeOf(TriggerSpec{})
	typeStep       = reflect.TypeOf(StepSpec{})
	typeRepository = reflect.TypeOf(RepositoryRef{})
	typeApproval   = reflect.TypeOf(ApprovalSpec{})
)

// triggerVariants splits TriggerSpec into the builtin and integration forms.
var triggerVariants = []defSpec{
	{name: "builtinTrigger", typ: typeTrigger, required: []string{"type"}, fields: []string{"type", "cron"}},
	{name: "integrationTrigger", typ: typeTrigger, required: []string{"integration", "event"}, fields: []string{"integration", "event", "filter"}, description: integrationTriggerDescription},
}

var objectDefs = []defSpec{
	{name: "repositoryRef", typ: typeRepository, required: []string{"ref"}, description: repositoryRefDescription},
	{name: "approval", typ: typeApproval, description: approvalDescription},
	{name: "step", typ: typeStep, required: []string{"id"}, description: stepDescription, oneOfRequired: []string{"image", "uses", "approval", "wait"}},
}

// refName maps a Go type to its $defs entry.
func refName(t reflect.Type) string {
	switch t {
	case typeTrigger:
		return "trigger"
	case typeStep:
		return "step"
	case typeRepository:
		return "repositoryRef"
	case typeApproval:
		return "approval"
	}
	return ""
}

// Schema builds the workflow JSON Schema as a JSON-compatible map.
func Schema() map[string]any {
	root := objectSchema(defSpec{typ: typeSpec, required: []string{"version", "name", "steps"}, description: schemaDescription})
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaID
	root["title"] = schemaTitle
	defs := map[string]any{}
	variants := make([]any, 0, len(triggerVariants))
	for _, v := range triggerVariants {
		defs[v.name] = objectSchema(v)
		variants = append(variants, map[string]any{"$ref": "#/$defs/" + v.name})
	}
	defs["trigger"] = map[string]any{"oneOf": variants}
	for _, d := range objectDefs {
		defs[d.name] = objectSchema(d)
	}
	root["$defs"] = defs
	return root
}

func objectSchema(d defSpec) map[string]any {
	keep := map[string]bool{}
	for _, f := range d.fields {
		keep[f] = true
	}
	props := map[string]any{}
	for i := 0; i < d.typ.NumField(); i++ {
		f := d.typ.Field(i)
		name := jsonName(f)
		if name == "" || (len(keep) > 0 && !keep[name]) {
			continue
		}
		props[name] = propertySchema(d.typ.Name()+"."+name, f.Type)
	}
	s := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(d.required) > 0 {
		s["required"] = toAny(d.required)
	}
	if d.description != "" {
		s["description"] = d.description
	}
	if len(d.oneOfRequired) > 0 {
		var alts []any
		for _, r := range d.oneOfRequired {
			alts = append(alts, map[string]any{"required": []any{r}})
		}
		s["oneOf"] = alts
	}
	return s
}

func propertySchema(key string, t reflect.Type) map[string]any {
	p := typeSchema(t)
	raw, ok := fieldAnnotations[key]
	if !ok {
		panic("workflow schema: no annotation for " + key)
	}
	var ann map[string]any
	if err := json.Unmarshal([]byte(raw), &ann); err != nil {
		panic(fmt.Sprintf("workflow schema: annotation %s: %v", key, err))
	}
	if ann["notype"] == true {
		delete(p, "type")
		delete(ann, "notype")
	}
	for k, v := range ann {
		p[k] = v
	}
	return p
}

func typeSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n := refName(t); n != "" {
		return map[string]any{"$ref": "#/$defs/" + n}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem())}
	case reflect.Map:
		s := map[string]any{"type": "object"}
		if t.Elem().Kind() != reflect.Interface {
			s["additionalProperties"] = typeSchema(t.Elem())
		}
		return s
	}
	panic("workflow schema: unsupported type " + t.String())
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

var (
	schemaOnce sync.Once
	schemaJSON []byte
)

// SchemaJSON returns the schema as indented JSON (cached).
func SchemaJSON() []byte {
	schemaOnce.Do(func() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(Schema()); err != nil {
			panic(err)
		}
		schemaJSON = buf.Bytes()
	})
	return schemaJSON
}

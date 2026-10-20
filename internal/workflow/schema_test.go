package workflow

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The committed schema must equal the generated one; run
// `go generate ./...` after changing the spec types or annotations.
func TestSchemaFileIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", "schema", "workflow.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, SchemaJSON()) {
		t.Fatal("schema/workflow.schema.json is stale: run `go generate ./...`")
	}
}

// Drift test: the schema generated from the Go types must be semantically
// equal to the blueprint contract.
func TestSchemaMatchesBlueprintContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "blueprint", "docs", "schemas", "workflow.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(SchemaJSON(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		diffJSON(t, "", want, got)
		t.Fatal("generated schema drifted from blueprint/docs/schemas/workflow.schema.json")
	}
}

func diffJSON(t *testing.T, path string, want, got any) {
	t.Helper()
	wm, wok := want.(map[string]any)
	gm, gok := got.(map[string]any)
	if wok && gok {
		for k := range wm {
			if _, ok := gm[k]; !ok {
				t.Errorf("%s/%s: missing in generated schema", path, k)
				continue
			}
			diffJSON(t, path+"/"+k, wm[k], gm[k])
		}
		for k := range gm {
			if _, ok := wm[k]; !ok {
				t.Errorf("%s/%s: extra in generated schema", path, k)
			}
		}
		return
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s: want %v, got %v", path, want, got)
	}
}

// Every annotation must belong to a real field, and every field of the
// spec types must be annotated (propertySchema panics otherwise).
func TestAnnotationsCoverFields(t *testing.T) {
	fields := map[string]bool{}
	for _, typ := range []reflect.Type{typeSpec, typeTrigger, typeStep, typeRepository} {
		for i := 0; i < typ.NumField(); i++ {
			fields[typ.Name()+"."+jsonName(typ.Field(i))] = true
		}
	}
	for k := range fieldAnnotations {
		if !fields[k] {
			t.Errorf("annotation %s has no field", k)
		}
	}
	covered := map[string]bool{}
	for _, v := range triggerVariants {
		for _, f := range v.fields {
			if covered[f] {
				t.Errorf("trigger field %s is in two variants", f)
			}
			covered[f] = true
		}
	}
	for i := 0; i < typeTrigger.NumField(); i++ {
		if !covered[jsonName(typeTrigger.Field(i))] {
			t.Errorf("TriggerSpec.%s is in no variant", jsonName(typeTrigger.Field(i)))
		}
	}
}

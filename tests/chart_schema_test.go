package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The chart validates each environment's embedded integrations.yaml with a copy
// of the integrations schema. A schema change must reach the chart too, or
// Helm would accept configs the services reject (and the reverse).
func TestChartValuesSchemaEmbedsIntegrationsSchema(t *testing.T) {
	root := repoRoot(t)
	source := mustReadJSONSchema(t, filepath.Join(root, "packages/config-schema/src/integrations.schema.json"))
	chart := mustReadJSONSchema(t, filepath.Join(root, "infra/helm/atrium/values.schema.json"))

	delete(source, "$schema")
	properties, _ := chart["properties"].(map[string]any)
	embedded, ok := properties["integrations"].(map[string]any)
	if !ok {
		t.Fatal("values.schema.json has no properties.integrations")
	}
	if !reflect.DeepEqual(source, embedded) {
		t.Fatal("infra/helm/atrium/values.schema.json properties.integrations differs from " +
			"packages/config-schema/src/integrations.schema.json (minus $schema); copy the schema across")
	}
}

// managedCollectors (ADR 0014) must accept the same collectors as
// integrations.collectors, so its items reference the embedded integrations
// schema's collector by that schema's $id instead of keeping a copy that
// could drift. Helm resolves the $id inside values.schema.json; a $ref it
// could not resolve there would be fetched over the network.
func TestChartValuesSchemaManagedCollectorsReuseCollectorSchema(t *testing.T) {
	root := repoRoot(t)
	source := mustReadJSONSchema(t, filepath.Join(root, "packages/config-schema/src/integrations.schema.json"))
	chart := mustReadJSONSchema(t, filepath.Join(root, "infra/helm/atrium/values.schema.json"))

	properties, _ := chart["properties"].(map[string]any)
	managed, _ := properties["managedCollectors"].(map[string]any)
	items, _ := managed["items"].(map[string]any)
	allOf, _ := items["allOf"].([]any)
	var refs []string
	for _, schema := range allOf {
		if ref, ok := schema.(map[string]any)["$ref"].(string); ok {
			refs = append(refs, ref)
		}
	}
	id, _ := source["$id"].(string)
	want := id + "#/properties/collectors/items"
	if len(refs) != 1 || refs[0] != want {
		t.Fatalf("values.schema.json managedCollectors.items.allOf $refs = %q, want [%q]", refs, want)
	}
	collectors, _ := source["properties"].(map[string]any)["collectors"].(map[string]any)
	if _, ok := collectors["items"].(map[string]any); !ok {
		t.Fatalf("%s does not resolve: the integrations schema has no properties.collectors.items", want)
	}
}

// The chart schema rejects unknown top-level keys, so a misspelt value
// (`--set dbColector.enabled=false`) fails instead of being ignored. Every
// top-level key that values.yaml or an environment's values file sets must
// therefore be a schema property, listed once: a duplicate, which a merge of
// two branches can leave behind, silently drops all but one of them.
func TestChartValuesSchemaListsTopLevelKeys(t *testing.T) {
	root := repoRoot(t)
	schemaPath := filepath.Join(root, "infra/helm/atrium/values.schema.json")
	schema := mustReadJSONSchema(t, schemaPath)
	if schema["additionalProperties"] != false {
		t.Fatal("values.schema.json must set additionalProperties: false at the top level")
	}
	properties, _ := schema["properties"].(map[string]any)

	files, err := filepath.Glob(filepath.Join(root, "infra/helm/values/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(root, "infra/helm/atrium/values.yaml"))
	for _, file := range files {
		var values map[string]any
		readYAML(t, file, &values)
		rel, _ := filepath.Rel(root, file)
		if len(values) == 0 {
			t.Errorf("%s sets no values; the test is not reading it", rel)
		}
		for key := range values {
			if _, ok := properties[key]; !ok {
				t.Errorf("%s sets %s, which values.schema.json does not list; add it to the top-level properties", rel, key)
			}
		}
	}

	content, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range duplicateKeys(t, content, "properties") {
		t.Errorf("values.schema.json lists the top-level property %s more than once", key)
	}
}

// duplicateKeys returns the keys that appear more than once in the object at
// a top-level key of a JSON document.
func duplicateKeys(t *testing.T, content []byte, at string) []string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(doc[at]))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("%s is not an object", at)
	}
	seen := map[string]bool{}
	var duplicates []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		key := token.(string)
		if seen[key] {
			duplicates = append(duplicates, key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return duplicates
}

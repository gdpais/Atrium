// Package configschema holds the JSON Schemas of Atrium's configuration,
// src/*.schema.json, for the code that checks configuration at run time.
//
// So far that is the config of each telemetry source type, which the API
// checks before it stores a source (ADR 0017 §1): one schema per type in
// src/telemetry-sources, named after the type; and a collector of the
// integrations file, which the API checks every collector it renders from
// probe assignments against (ADR 0014 §2); and an environment's inventory
// file (ADR 0021), which `api inventory check` and the API check.
package configschema

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed src/telemetry-sources/*.schema.json
var sources embed.FS

//go:embed src/integrations.schema.json
var integrations []byte

//go:embed src/inventory.schema.json
var inventory []byte

// integrationsID is the integrations schema's $id.
const integrationsID = "https://atrium.local/schemas/integrations.schema.json"

// inventoryID is the inventory schema's $id.
const inventoryID = "https://atrium.local/schemas/inventory.schema.json"

const sourcesDir = "src/telemetry-sources"

// CredentialRefFormat is the format a schema gives a property that names a
// credential, such as {"type": "string", "format": "credential-ref"}: a
// reference such as env/finance-prod or secret/api-postgres, resolved at run
// time (package credentials), never the credential itself.
const CredentialRefFormat = "credential-ref"

// CredentialRef matches a credential reference as the integrations file and
// the API's own settings take one: env/, kv/ or secret/, then letters,
// digits, ".", "-", "_" and "/".
var CredentialRef = regexp.MustCompile(`^(env|kv|secret)/[A-Za-z0-9._/-]{1,200}$`)

// SourceTypes returns the telemetry source types that have a schema,
// sorted.
func SourceTypes() []string {
	entries, err := fs.ReadDir(sources, sourcesDir)
	if err != nil {
		panic(err) // the directory is embedded
	}
	var types []string
	for _, e := range entries {
		types = append(types, strings.TrimSuffix(e.Name(), ".schema.json"))
	}
	sort.Strings(types)
	return types
}

// SourceSchemaJSON returns the schema of sourceType's config, as written.
func SourceSchemaJSON(sourceType string) ([]byte, error) {
	data, err := sources.ReadFile(path.Join(sourcesDir, sourceType+".schema.json"))
	if err != nil {
		return nil, fmt.Errorf("no schema for telemetry source type %q", sourceType)
	}
	return data, nil
}

// SourceSchemas compiles the schema of every source type, by type, with
// formats asserted and the credential-ref format known.
func SourceSchemas() (map[string]*jsonschema.Schema, error) {
	compiler := newCompiler()
	out := map[string]*jsonschema.Schema{}
	for _, sourceType := range SourceTypes() {
		data, err := SourceSchemaJSON(sourceType)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s schema: %w", sourceType, err)
		}
		url := "embed:///" + sourceType + ".schema.json"
		if err := compiler.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("%s schema: %w", sourceType, err)
		}
		if out[sourceType], err = compiler.Compile(url); err != nil {
			return nil, fmt.Errorf("%s schema: %w", sourceType, err)
		}
	}
	return out, nil
}

// CollectorSchema compiles the schema of one collector of the integrations
// file: integrations.schema.json#/properties/collectors/items.
func CollectorSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(integrations))
	if err != nil {
		return nil, fmt.Errorf("integrations schema: %w", err)
	}
	compiler := newCompiler()
	if err := compiler.AddResource(integrationsID, doc); err != nil {
		return nil, fmt.Errorf("integrations schema: %w", err)
	}
	schema, err := compiler.Compile(integrationsID + "#/properties/collectors/items")
	if err != nil {
		return nil, fmt.Errorf("integrations schema: %w", err)
	}
	return schema, nil
}

// InventorySchema compiles the schema of an inventory file.
func InventorySchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(inventory))
	if err != nil {
		return nil, fmt.Errorf("inventory schema: %w", err)
	}
	compiler := newCompiler()
	if err := compiler.AddResource(inventoryID, doc); err != nil {
		return nil, fmt.Errorf("inventory schema: %w", err)
	}
	schema, err := compiler.Compile(inventoryID)
	if err != nil {
		return nil, fmt.Errorf("inventory schema: %w", err)
	}
	return schema, nil
}

// newCompiler returns a compiler that asserts formats, credential-ref
// included.
func newCompiler() *jsonschema.Compiler {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{Name: CredentialRefFormat, Validate: func(v any) error {
		s, ok := v.(string)
		if !ok || CredentialRef.MatchString(s) {
			return nil
		}
		return fmt.Errorf("not a credential reference such as env/finance-prod")
	}})
	return compiler
}

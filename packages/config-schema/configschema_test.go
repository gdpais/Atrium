package configschema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOutSystemsSourceConfig(t *testing.T) {
	schemas, err := SourceSchemas()
	if err != nil {
		t.Fatal(err)
	}
	schema := schemas["outsystems"]
	if schema == nil {
		t.Fatalf("no outsystems schema among %v", SourceTypes())
	}
	for _, ok := range []string{`{"application_name":"Claims"}`, `{"application_name":"Claims Portal 2"}`, `{"application_name":"Ação"}`} {
		if err := schema.Validate(mustJSON(t, ok)); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{}`, `{"application_name":""}`, `{"application_name":" Claims"}`, `{"application_name":"Claims "}`,
		`{"application_name":"Cla\u0000ims"}`, `{"application_name":42}`,
		`{"application_name":"Claims","password":"hunter2"}`, `{"application_name":"` + strings.Repeat("a", 201) + `"}`, `[]`,
	} {
		if err := schema.Validate(mustJSON(t, bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Sources store credential references only, never credentials: every
// source type's schema is closed at every level, and a property named like
// a credential is a credential-ref string. A schema that breaks this fails
// here before the API could store a password in PostgreSQL.
func TestSourceSchemasHoldReferencesOnly(t *testing.T) {
	for _, sourceType := range SourceTypes() {
		data, err := SourceSchemaJSON(sourceType)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		checkClosed(t, sourceType, "", doc)
	}
}

func checkClosed(t *testing.T, sourceType, at string, node map[string]any) {
	t.Helper()
	if node["type"] == "object" && node["additionalProperties"] != false {
		t.Errorf("%s schema%s: an object without additionalProperties: false could hold anything, a secret included", sourceType, at)
	}
	props, _ := node["properties"].(map[string]any)
	for name, p := range props {
		prop, _ := p.(map[string]any)
		lower := strings.ToLower(name)
		for _, word := range []string{"password", "secret", "token", "credential", "key"} {
			if strings.Contains(lower, word) && (prop["format"] != CredentialRefFormat || !strings.HasSuffix(lower, "_ref")) {
				t.Errorf("%s schema%s.%s: a property named like a credential must be a *_ref with format %s", sourceType, at, name, CredentialRefFormat)
			}
		}
		checkClosed(t, sourceType, at+"."+name, prop)
	}
}

// A credential-ref property takes a reference and refuses anything else,
// a password most of all.
func TestCredentialRefFormat(t *testing.T) {
	compiler := newCompiler()
	doc := mustJSON(t, `{"type": "string", "format": "credential-ref"}`)
	if err := compiler.AddResource("mem:///ref.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("mem:///ref.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"env/finance-prod", "secret/api-postgres", "kv/sqlserver-default", "env/SQL_PASSWORD", "kv/team/claims.prod"} {
		if err := schema.Validate(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "hunter2", "env/", "/x", "Env/x", "vault/x", "vault:api-redis", "env/x y", "postgres://u:p@h/db"} {
		if err := schema.Validate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(s)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

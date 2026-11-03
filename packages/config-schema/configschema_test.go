package configschema

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		`{"application_name":"Claims\u00a0"}`, `{"application_name":"\u2003Claims"}`, `{"application_name":"Claims\u3000"}`,
		`{"application_name":"Claims\u200b"}`, `{"application_name":"Cla\u200bims"}`, `{"application_name":"Claims\u00a0Portal"}`,
		`{"application_name":"Claims\tPortal"}`, `{"application_name":"Claims  "}`,
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
		checkClosed(t.Errorf, sourceType, "", doc)
	}
}

// credentialWords mark a property name as a credential's.
var credentialWords = []string{"password", "passwd", "pass", "pwd", "secret", "token", "credential", "key", "auth", "dsn", "connection_string"}

// checkClosed walks every subschema of node, wherever it sits ($defs,
// items, allOf, oneOf, anyOf, if/then/else, not, properties): each one that
// describes an object lists its members and allows no other, none takes
// members by pattern, and a member named like a credential is a reference.
func checkClosed(report func(format string, args ...any), sourceType, at string, node any) {
	switch n := node.(type) {
	case []any:
		for i, v := range n {
			checkClosed(report, sourceType, fmt.Sprintf("%s[%d]", at, i), v)
		}
	case map[string]any:
		_, hasProps := n["properties"]
		if (n["type"] == "object" || hasProps) && n["additionalProperties"] != false {
			report("%s schema%s: an object without additionalProperties: false could hold anything, a secret included", sourceType, at)
		}
		for _, open := range []string{"patternProperties", "unevaluatedProperties", "propertyNames"} {
			if v, ok := n[open]; ok && v != false {
				report("%s schema%s: %s lets an object take members its schema does not name", sourceType, at, open)
			}
		}
		props, _ := n["properties"].(map[string]any)
		for name, p := range props {
			prop, _ := p.(map[string]any)
			lower := strings.ToLower(name)
			for _, word := range credentialWords {
				if strings.Contains(lower, word) && (prop["format"] != CredentialRefFormat || !strings.HasSuffix(lower, "_ref")) {
					report("%s schema%s.%s: a property named like a credential must be a *_ref with format %s", sourceType, at, name, CredentialRefFormat)
				}
			}
		}
		for key, v := range n {
			checkClosed(report, sourceType, at+"/"+key, v)
		}
	}
}

// The check itself catches what it is for: a schema that hides an open
// object under $defs, or names a password plainly.
func TestCheckClosedCatchesOpenSchemas(t *testing.T) {
	for name, schema := range map[string]string{
		"open object under $defs": `{"type":"object","additionalProperties":false,"properties":{"headers":{"$ref":"#/$defs/h"}},"$defs":{"h":{"type":"object","additionalProperties":{"type":"string"}}}}`,
		"pattern properties":      `{"type":"object","additionalProperties":false,"patternProperties":{".*":{"type":"string"}}}`,
		"a password":              `{"type":"object","additionalProperties":false,"properties":{"db_pass":{"type":"string"}}}`,
		"properties without type": `{"properties":{"a":{"type":"string"}}}`,
	} {
		var doc any
		if err := json.Unmarshal([]byte(schema), &doc); err != nil {
			t.Fatal(err)
		}
		caught := false
		checkClosed(func(string, ...any) { caught = true }, "probe", "", doc)
		if !caught {
			t.Errorf("%s: not caught", name)
		}
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

// Package audittest checks audit records in tests: against their JSON
// Schema, packages/telemetry-contracts/src/audit_record.schema.json, and for
// secrets (ADR 0016 §6).
package audittest

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaPath is the record's schema, from the repository root.
const SchemaPath = "packages/telemetry-contracts/src/audit_record.schema.json"

var (
	compileOnce sync.Once
	schema      *jsonschema.Schema
	compileErr  error
)

// Validate fails t unless record, a record as JSON, is valid against the
// schema, its formats (uuid, date-time) included.
func Validate(t testing.TB, record []byte) {
	t.Helper()
	compileOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.AssertFormat()
		schema, compileErr = compiler.Compile(filepath.Join(repoRoot(t), SchemaPath))
	})
	if compileErr != nil {
		t.Fatalf("compile %s: %v", SchemaPath, compileErr)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
	if err != nil {
		t.Fatalf("record is not JSON: %v\n%s", err, record)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("record is not valid against %s: %v\n%s", SchemaPath, err, record)
	}
}

// secretShapes match secrets whatever a test declared: a PHC password hash.
var secretShapes = []*regexp.Regexp{regexp.MustCompile(`\$argon2(id|i|d)\$`)}

// NoSecrets fails t if record, a record as JSON, contains any of secrets,
// or anything shaped like a password hash. A PHC hash among secrets is also
// looked for by its salt and its digest alone, so a record that splits one
// up is caught too. Empty secrets are skipped.
func NoSecrets(t testing.TB, record []byte, secrets ...string) {
	t.Helper()
	text := string(record)
	for _, secret := range expand(secrets) {
		if strings.Contains(text, secret) {
			t.Errorf("audit record contains a secret (%d characters, starting %q):\n%s", len(secret), secret[:min(len(secret), 4)], text)
		}
	}
	for _, shape := range secretShapes {
		if loc := shape.FindStringIndex(text); loc != nil {
			t.Errorf("audit record contains something shaped like a password hash at %d:\n%s", loc[0], text)
		}
	}
}

// expand returns secrets, and the salt and digest of each PHC string among
// them.
func expand(secrets []string) []string {
	var out []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = append(out, secret)
		if strings.HasPrefix(secret, "$") {
			for part := range strings.SplitSeq(secret, "$") {
				// The salt and the digest: unpadded base64 of 16 bytes or more.
				if len(part) >= 22 && !strings.ContainsAny(part, "=,") {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

// repoRoot walks up from the working directory to the directory of go.mod.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

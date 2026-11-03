package inventory

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	api "heartbeat/apps/api"
	"heartbeat/internal/config"
	"heartbeat/internal/credentials"
)

func validTarget() NewDatabaseTarget {
	return NewDatabaseTarget{
		EnvironmentID: uuid.New(), Name: "finance-prod", Host: "sql01.finance.internal",
		CredentialRef: "env/finance-prod",
	}
}

func TestNewDatabaseTargetDefaults(t *testing.T) {
	got, err := newDatabaseTarget(validTarget())
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine != "sqlserver" || got.Port != 1433 || got.Status != StatusActive || got.DatabaseName != "" || len(got.Labels) != 0 {
		t.Fatalf("newDatabaseTarget() = %+v, want sqlserver, port 1433, active, no database and no labels", got)
	}
	for host, want := range map[string]string{"FD00:0:0::7": "fd00::7", "fd00:0000::0007": "fd00::7"} {
		got, err := targetHost(host)
		if err != nil || got != want {
			t.Errorf("targetHost(%q) = %q, %v; want %q", host, got, err, want)
		}
	}
	in := validTarget()
	in.Host = "SQL01.Finance.Internal"
	in.DatabaseName = "  Finance  "
	in.Port = 14330
	if got, err = newDatabaseTarget(in); err != nil || got.Host != "sql01.finance.internal" || got.DatabaseName != "Finance" || got.Port != 14330 {
		t.Fatalf("newDatabaseTarget(%+v) = %+v, %v; want the host in lower case and the database name trimmed", in, got, err)
	}
}

func TestNewDatabaseTargetRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*NewDatabaseTarget)
		field  string
	}{
		{"no environment", func(n *NewDatabaseTarget) { n.EnvironmentID = uuid.Nil }, "environment_id"},
		{"another engine", func(n *NewDatabaseTarget) { n.Engine = "postgres" }, "engine"},
		{"name in upper case", func(n *NewDatabaseTarget) { n.Name = "Finance-Prod" }, "name"},
		{"name with a space", func(n *NewDatabaseTarget) { n.Name = "finance prod" }, "name"},
		{"no name", func(n *NewDatabaseTarget) { n.Name = "" }, "name"},
		{"no host", func(n *NewDatabaseTarget) { n.Host = "" }, "host"},
		{"host with a port", func(n *NewDatabaseTarget) { n.Host = "sql01:1433" }, "host"},
		{"host with credentials", func(n *NewDatabaseTarget) { n.Host = "sa@sql01" }, "host"},
		{"host with an instance", func(n *NewDatabaseTarget) { n.Host = `sql01\finance` }, "host"},
		{"bracketed IPv6 host", func(n *NewDatabaseTarget) { n.Host = "[::1]" }, "host"},
		{"host too long", func(n *NewDatabaseTarget) { n.Host = strings.Repeat("a", maxHostBytes+1) }, "host"},
		{"port too high", func(n *NewDatabaseTarget) { n.Port = 65536 }, "port"},
		{"negative port", func(n *NewDatabaseTarget) { n.Port = -1 }, "port"},
		{"database name too long", func(n *NewDatabaseTarget) { n.DatabaseName = strings.Repeat("d", maxDatabaseNameRunes+1) }, "database_name"},
		{"database name with a control character", func(n *NewDatabaseTarget) { n.DatabaseName = "fin\x00ance" }, "database_name"},
		{"no credential reference", func(n *NewDatabaseTarget) { n.CredentialRef = "" }, "credential_ref"},
		{"status unknown", func(n *NewDatabaseTarget) { n.Status = "paused" }, "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validTarget()
			tc.change(&in)
			_, err := newDatabaseTarget(in)
			var invalid *InvalidError
			if !errors.As(err, &invalid) || invalid.Field != tc.field {
				t.Fatalf("newDatabaseTarget() = %v, want an *InvalidError for %s", err, tc.field)
			}
		})
	}
}

// A credential reference names a key of the collector's credentials Secret;
// a credential pasted in its place is refused, and the error does not
// repeat it.
func TestCredentialRef(t *testing.T) {
	for _, ref := range []string{"env/finance-prod", "kv/sqlserver-default", "secret/api-postgres", "env/0-orders", "env/" + strings.Repeat("a", 200)} {
		if _, err := credentialRef(ref); err != nil {
			t.Errorf("credentialRef(%q) = %v, want it accepted", ref, err)
		}
	}
	for _, ref := range []string{
		"", "finance-prod", "vault/finance", "env/", "env/-finance", "env/" + strings.Repeat("a", 201),
		// Other spellings of keys a reference above has: they would reach
		// its credential while looking like another reference.
		"env/Finance-Prod", "env/finance.prod", "env/finance_prod", "env/HEARTBEAT_SQLSERVER", "secret/api.postgres",
		"env/finance prod", "env/finance/prod", "env/sa:not-a-secret", "sa:not-a-secret", "env/sa@sql01",
		urlWithCredential("vault.internal/finance"), " env/finance-prod", "env/finance-prod\n", "env/finançe",
	} {
		_, err := credentialRef(ref)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != "credential_ref" {
			t.Errorf("credentialRef(%q) = %v, want an *InvalidError for credential_ref", ref, err)
			continue
		}
		// The credential-shaped ones: the others are parts of the example.
		if strings.ContainsAny(ref, ":@") && strings.Contains(err.Error(), ref) {
			t.Errorf("credentialRef(%q) = %q: the error repeats the value", ref, err)
		}
	}
}

// What the API accepts the collector accepts too: every reference it
// takes passes the collector's own check and maps to a key that is a valid
// Kubernetes Secret key and environment variable name, and every host
// passes the collector's host check.
func TestTargetChecksAreNoLooserThanTheCollectors(t *testing.T) {
	for _, ref := range []string{"env/finance-prod", "kv/a", "secret/a-b--c", "env/" + strings.Repeat("x", 200)} {
		got, err := credentialRef(ref)
		if err != nil {
			t.Fatalf("credentialRef(%q) = %v", ref, err)
		}
		if !config.ValidSecretRef(got) {
			t.Errorf("the API accepts %q, which the collector refuses", ref)
		}
		key := credentials.Key(got)
		if len(key) > 253 || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
			t.Errorf("credentials.Key(%q) = %q: not a valid Secret key and variable name", ref, key)
		}
	}
	for _, host := range []string{"sql01.finance.internal", "10.0.0.7", "fd00::7", "SQL_01"} {
		got, err := targetHost(host)
		if err != nil {
			t.Fatalf("targetHost(%q) = %v", host, err)
		}
		if !config.ValidHost(got) {
			t.Errorf("the API accepts host %q, which the collector refuses", host)
		}
	}
}

func TestApplyDatabaseTargetChange(t *testing.T) {
	before, err := newDatabaseTarget(validTarget())
	if err != nil {
		t.Fatal(err)
	}
	host, port, db, ref, status := "SQL02.finance.internal", 1434, "", "kv/finance-prod", StatusDisabled
	labels := map[string]string{"tier": "gold"}
	after, err := applyDatabaseTargetChange(before, DatabaseTargetChange{Host: &host, Port: &port, DatabaseName: &db, CredentialRef: &ref, Status: &status, Labels: &labels})
	if err != nil {
		t.Fatal(err)
	}
	if after.Host != "sql02.finance.internal" || after.Port != 1434 || after.CredentialRef != ref || after.Status != status || after.Labels["tier"] != "gold" {
		t.Errorf("applyDatabaseTargetChange() = %+v", after)
	}
	if after.Name != before.Name || after.Engine != before.Engine || after.EnvironmentID != before.EnvironmentID {
		t.Errorf("applyDatabaseTargetChange() changed the name, engine or environment: %+v", after)
	}
	if sameDatabaseTarget(before, after) {
		t.Error("sameDatabaseTarget() = true for a changed target")
	}
	unchanged, err := applyDatabaseTargetChange(before, DatabaseTargetChange{})
	if err != nil || !sameDatabaseTarget(before, unchanged) {
		t.Errorf("an empty change: %+v, %v; want the target as it was", unchanged, err)
	}
	bad := "user:password"
	if _, err := applyDatabaseTargetChange(before, DatabaseTargetChange{CredentialRef: &bad}); err == nil {
		t.Error("applyDatabaseTargetChange() accepted a credential as the reference")
	}
}

// The spec's patterns and limits are the service's.
func TestTargetPatternsMatchTheSpec(t *testing.T) {
	spec := specSchemas(t)
	if got := spec["CredentialRef"].Pattern; got != CredentialRefPattern {
		t.Errorf("spec CredentialRef pattern = %q, want %q", got, CredentialRefPattern)
	}
	if got := spec["TargetName"].Pattern; got != SlugPattern {
		t.Errorf("spec TargetName pattern = %q, want %q", got, SlugPattern)
	}
	if got := spec["Host"].MaxLength; got != maxHostBytes {
		t.Errorf("spec Host maxLength = %d, want %d", got, maxHostBytes)
	}
	if got := spec["DatabaseName"].MaxLength; got != maxDatabaseNameRunes {
		t.Errorf("spec DatabaseName maxLength = %d, want %d", got, maxDatabaseNameRunes)
	}
	if got := spec["DatabaseEngine"].Enum; strings.Join(got, ",") != strings.Join(Engines, ",") {
		t.Errorf("spec DatabaseEngine enum = %v, want %v", got, Engines)
	}
}

// specSchema is what TestTargetPatternsMatchTheSpec reads of a schema.
type specSchema struct {
	Pattern   string   `yaml:"pattern"`
	MaxLength int      `yaml:"maxLength"`
	Enum      []string `yaml:"enum"`
}

func specSchemas(t *testing.T) map[string]specSchema {
	t.Helper()
	var doc struct {
		Components struct {
			Schemas map[string]specSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Components.Schemas
}

// urlWithCredential returns a URL that embeds a made-up credential, built
// at run time so that no credential-shaped literal sits in the source for
// secret scanners to flag.
func urlWithCredential(rest string) string {
	return "https://" + "sa" + ":" + "not-a-secret" + "@" + rest
}

// Every key has one spelling: two references the API accepts never resolve
// to the same credential, so a reference that looks new never reaches
// another's. Checked over every name of up to three characters from the
// alphabet, under every prefix.
func TestCredentialRefsNeverShareAKey(t *testing.T) {
	// Upper case, '.' and '_' too: the spellings the collector folds into
	// one key.
	alphabet := "aA0-._"
	names, last := []string{}, []string{""}
	for range 3 {
		var longer []string
		for _, n := range last {
			for _, c := range alphabet {
				longer = append(longer, n+string(c))
			}
		}
		names, last = append(names, longer...), longer
	}
	owner := map[string]string{}
	accepted := 0
	for _, prefix := range []string{"kv/", "secret/", "env/"} {
		for _, n := range names {
			ref := prefix + n
			if _, err := credentialRef(ref); err != nil {
				continue
			}
			accepted++
			key := credentials.Key(ref)
			if other, ok := owner[key]; ok {
				t.Fatalf("%q and %q both resolve to %s", other, ref, key)
			}
			owner[key] = ref
		}
	}
	if accepted < 50 {
		t.Fatalf("only %d references accepted: the sample checks too little", accepted)
	}
}

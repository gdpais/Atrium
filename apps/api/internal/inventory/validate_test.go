package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	api "heartbeat/apps/api"
)

// The slug is the environment's name in the collectors' config and in
// telemetry (ADR 0017 §2): every value the repository's collectors use
// passes, and anything that would not work as a label value, a URL segment
// and a Kubernetes name fails.
func TestSlug(t *testing.T) {
	for _, ok := range []string{"prod", "production", "local", "local-dev", "dev", "a", "0", "eu-west-1", strings.Repeat("a", 63)} {
		if _, err := slug(ok); err != nil {
			t.Errorf("slug(%q) = %v, want it accepted", ok, err)
		}
	}
	for _, bad := range []string{"", "Prod", "prod ", " prod", "-prod", "prod-", "pr_od", "pr.od", "prød", "prod/eu", strings.Repeat("a", 64)} {
		_, err := slug(bad)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != "slug" {
			t.Errorf("slug(%q) = %v, want an InvalidError for slug", bad, err)
		}
	}
}

func TestIdentifier(t *testing.T) {
	for _, ok := range []string{"outsystems_traditional", "module", "screen_flow", "dotnet8", strings.Repeat("a", 63)} {
		if _, err := identifier("platform", ok); err != nil {
			t.Errorf("identifier(%q) = %v, want it accepted", ok, err)
		}
	}
	for _, bad := range []string{"", "OutSystems", "8dotnet", "_module", "screen-flow", "screen flow", strings.Repeat("a", 64)} {
		if _, err := identifier("platform", bad); err == nil {
			t.Errorf("identifier(%q) accepted, want an error", bad)
		}
	}
}

func TestName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Claims Portal", "Claims Portal"},
		{"  Claims Portal \t", "Claims Portal"},
		{"Ação de Seguros", "Ação de Seguros"},
		{strings.Repeat("é", 200), strings.Repeat("é", 200)},
	} {
		got, err := name("name", tc.in)
		if err != nil || got != tc.want {
			t.Errorf("name(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", strings.Repeat("é", 201), "Claims\x00Portal", "Claims\nPortal", "\xff"} {
		if _, err := name("name", bad); err == nil {
			t.Errorf("name(%q) accepted, want an error", bad)
		}
	}
}

func TestDescriptionAllowsLineBreaks(t *testing.T) {
	if got, err := text("description", "Line one.\nLine two.\tTabbed.", maxDescriptionRunes, true); err != nil || got != "Line one.\nLine two.\tTabbed." {
		t.Fatalf("text = %q, %v", got, err)
	}
	if _, err := text("description", "bell\a", maxDescriptionRunes, true); err == nil {
		t.Fatal("a control character other than a line break or a tab was accepted")
	}
	if _, err := text("description", strings.Repeat("x", maxDescriptionRunes+1), maxDescriptionRunes, true); err == nil {
		t.Fatal("2001 characters accepted")
	}
}

func TestOneOf(t *testing.T) {
	if got, err := oneOf("status", "", StatusActive, statuses); err != nil || got != StatusActive {
		t.Fatalf("empty status = %q, %v; want the default", got, err)
	}
	if _, err := oneOf("status", "archived", StatusActive, statuses); err == nil || !strings.Contains(err.Error(), "active, disabled") {
		t.Fatalf("unknown status = %v, want an error listing the values", err)
	}
	if _, err := oneOf("type", "", "", environmentTypes); err == nil {
		t.Fatal("an empty value without a default was accepted")
	}
}

func TestLabels(t *testing.T) {
	got, err := labels(nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("labels(nil) = %v, %v; want an empty map", got, err)
	}
	in := map[string]string{"team": "claims", "cost-center": ""}
	got, err = labels(in)
	if err != nil || len(got) != 2 {
		t.Fatalf("labels = %v, %v", got, err)
	}
	got["team"] = "changed"
	if in["team"] != "claims" {
		t.Fatal("labels returned the caller's map, not a copy")
	}
	many := map[string]string{}
	for i := range maxLabels + 1 {
		many[fmt.Sprint("k", i)] = "v"
	}
	for name, bad := range map[string]map[string]string{
		"too many":         many,
		"empty key":        {"": "v"},
		"long key":         {strings.Repeat("k", 64): "v"},
		"long value":       {"k": strings.Repeat("v", 257)},
		"control in key":   {"k\n": "v"},
		"control in value": {"k": "v\x00"},
	} {
		if _, err := labels(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The spec documents the patterns the service enforces: oapi-codegen does
// not check them, so the two must not drift.
func TestPatternsMatchTheSpec(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Pattern string `yaml:"pattern"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	for schema, want := range map[string]string{
		"Slug":          SlugPattern,
		"Platform":      IdentifierPattern,
		"ComponentType": IdentifierPattern,
	} {
		if got := doc.Components.Schemas[schema].Pattern; got != want {
			t.Errorf("spec schema %s pattern = %q, want %q", schema, got, want)
		}
	}
}

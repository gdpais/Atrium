package main

import (
	"strings"
	"testing"
)

func TestCertificateTrustRequiresExplicitOptIn(t *testing.T) {
	const key = "ATRIUM_DB_COLLECTOR_SQLSERVER_TRUST_SERVER_CERTIFICATE"
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"0", false}, {"invalid", false},
		{"true", true}, {"TRUE", true}, {"1", true}, {"on", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(key, tc.value)
			if got := boolEnv(key, false); got != tc.want {
				t.Fatalf("boolEnv(%q) = %t, want %t", tc.value, got, tc.want)
			}
		})
	}
}

func TestCheckConfigCommand(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := command([]string{"check-config", "-f", "../../../../config/integrations.yaml"}, &stdout, &stderr); code != 0 {
		t.Fatalf("example config: exit %d, %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid") {
		t.Fatalf("stdout %q", stdout.String())
	}
	stderr.Reset()
	if code := command([]string{"check-config", "-f", "missing.yaml"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "missing.yaml") {
		t.Fatalf("missing file: exit %d, %q", code, stderr.String())
	}
	if code := command([]string{"check"}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad usage: exit %d, want 2", code)
	}
}

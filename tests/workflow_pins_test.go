package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every remote action and reusable workflow runs pinned to a full commit SHA,
// with its exact version as a trailing comment (`owner/repo@<sha> # v1.2.3`).
// A tag can be moved to other code; a commit SHA cannot. Renovate
// (helpers:pinGitHubActionDigestsToSemver in renovate.json) keeps the SHA and
// the comment current. This test runs in `make test` and `make test-race`, so
// CI's test job, and through it ci-ok, fails on an unpinned action.
func TestWorkflowActionsArePinnedBySHA(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, dir := range []string{".github/workflows", ".github/actions"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && (strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	checked := 0
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(content, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, ref := range usesRefs(&doc) {
			checked++
			if problem := unpinnedReason(ref.value, ref.comment); problem != "" {
				t.Errorf("%s:%d: uses: %s: %s", rel, ref.line, ref.value, problem)
			}
		}
	}
	// Guards the guard: a parsing change that finds nothing must not pass.
	if checked == 0 {
		t.Fatal("found no uses: entries under .github; the check is not looking at the workflows")
	}
}

func TestUnpinnedReason(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	cases := []struct {
		value, comment string
		ok             bool
	}{
		{"actions/checkout@" + sha, "# v7.0.1", true},
		{"googleapis/release-please-action@" + sha, "# v5.0.0", true},
		{"owner/repo/.github/workflows/build.yml@" + sha, "# v1.2.3", true},
		{"owner/repo/sub/action@" + sha, "# 1.2.3", true},
		{"./.github/actions/setup", "", true},
		{"./.github/workflows/reusable.yml", "", true},
		{"docker://alpine:3.22@sha256:" + strings.Repeat("a", 64), "", true},
		{"actions/checkout@v7", "", false},
		{"actions/checkout@v7.0.1", "", false},
		{"actions/checkout@main", "", false},
		{"actions/checkout", "", false},
		{"actions/checkout@" + sha[:7], "# v7.0.1", false},
		{"actions/checkout@" + strings.ToUpper(sha), "# v7.0.1", false},
		{"actions/checkout@" + sha, "", false},
		{"actions/checkout@" + sha, "# v7", false},
		{"owner/repo/.github/workflows/build.yml@main", "", false},
		{"docker://alpine:3.22", "", false},
	}
	for _, c := range cases {
		got := unpinnedReason(c.value, c.comment)
		if (got == "") != c.ok {
			t.Errorf("unpinnedReason(%q, %q) = %q, want ok=%v", c.value, c.comment, got, c.ok)
		}
	}
}

var (
	commitSHA     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	exactVersion  = regexp.MustCompile(`^#\s*v?\d+\.\d+\.\d+\b`)
	dockerDigest  = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
	remoteUsesRef = regexp.MustCompile(`^[^@\s]+/[^@\s]+@([^@\s]+)$`)
)

// unpinnedReason returns why a uses: value is not pinned, or "" when it is.
func unpinnedReason(value, comment string) string {
	switch {
	case strings.HasPrefix(value, "./"):
		return "" // an action or reusable workflow in this repository
	case strings.HasPrefix(value, "docker://"):
		if dockerDigest.MatchString(value) {
			return ""
		}
		return "pin the image by digest (docker://image:tag@sha256:...)"
	}
	m := remoteUsesRef.FindStringSubmatch(value)
	if m == nil {
		return "expected owner/repo@<40-character commit SHA>"
	}
	if !commitSHA.MatchString(m[1]) {
		return "pin to the full 40-character commit SHA, with the version as a comment: owner/repo@<sha> # v1.2.3"
	}
	if !exactVersion.MatchString(strings.TrimSpace(comment)) {
		return "add the exact version the SHA points to as a trailing comment (# v1.2.3), so Renovate can update it"
	}
	return ""
}

type usesRef struct {
	value, comment string
	line           int
}

// usesRefs collects every `uses:` scalar in a YAML document: step actions and
// job-level reusable workflows alike.
func usesRefs(n *yaml.Node) []usesRef {
	var out []usesRef
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "uses" && val.Kind == yaml.ScalarNode {
				comment := val.LineComment
				if comment == "" {
					comment = key.LineComment
				}
				out = append(out, usesRef{value: val.Value, comment: comment, line: val.Line})
			}
		}
	}
	for _, child := range n.Content {
		out = append(out, usesRefs(child)...)
	}
	return out
}

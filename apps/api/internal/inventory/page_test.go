package inventory

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	api "atrium/apps/api"
)

func TestPageLimits(t *testing.T) {
	for _, tc := range []struct {
		limit, want int
		ok          bool
	}{
		{0, DefaultLimit, true},
		{1, 1, true},
		{MaxLimit, MaxLimit, true},
		{MaxLimit + 1, 0, false},
		{-1, 0, false},
	} {
		got, _, err := Page{Limit: tc.limit}.resolve()
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("limit %d = %d, %v; want %d, ok %v", tc.limit, got, err, tc.want, tc.ok)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	_, after, err := Page{Cursor: cursor("Claims Portal", id)}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if after == nil || after.Name != "Claims Portal" || after.ID != id {
		t.Fatalf("cursor decodes to %+v, want Claims Portal and %s", after, id)
	}
}

func TestCursorRefusesWhatTheAPIDidNotReturn(t *testing.T) {
	for _, bad := range []string{
		"not base64!",
		base64.RawURLEncoding.EncodeToString([]byte("not json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"n":"x"}`)),            // no ID
		base64.RawURLEncoding.EncodeToString([]byte(`{"n":"x","i":"nope"}`)), // not a UUID
	} {
		if _, _, err := (Page{Cursor: bad}).resolve(); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestPageCutsAndPointsAtTheLastItem(t *testing.T) {
	type item struct {
		name string
		id   uuid.UUID
	}
	rows := []item{{"a", uuid.New()}, {"b", uuid.New()}, {"c", uuid.New()}}
	at := func(i item) (string, uuid.UUID) { return i.name, i.id }

	got, next := page(rows, 2, at)
	if len(got) != 2 || next != cursor("b", rows[1].id) {
		t.Fatalf("page of 3 rows with limit 2 = %d rows, next %q; want 2 and b's cursor", len(got), next)
	}
	got, next = page(rows, 3, at)
	if len(got) != 3 || next != "" {
		t.Fatalf("last page = %d rows, next %q; want 3 and none", len(got), next)
	}
}

// A cursor whose name no list could have returned is refused as the
// client's error: PostgreSQL would refuse a NUL in text, and the request
// would fail as a server error.
func TestCursorRefusesANameNoListHolds(t *testing.T) {
	for _, name := range []string{"Claims\x00", "Claims\n", strings.Repeat("a", maxNameRunes+1)} {
		raw, err := json.Marshal(position{Name: name, ID: uuid.New()})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := (Page{Cursor: base64.RawURLEncoding.EncodeToString(raw)}).resolve(); err == nil {
			t.Errorf("cursor with name %q accepted", name)
		}
	}
}

// Every cursor the API can issue fits the spec's Cursor maxLength, so a
// client that checks requests against the spec can send it back.
func TestCursorsFitTheSpec(t *testing.T) {
	var doc struct {
		Components struct {
			Parameters map[string]struct {
				Schema struct {
					MaxLength int `yaml:"maxLength"`
				} `yaml:"schema"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	limit := doc.Components.Parameters["Cursor"].Schema.MaxLength
	if limit == 0 {
		t.Fatal("the spec's Cursor parameter has no maxLength")
	}
	for _, r := range []string{"a", "é", "理", "🦀", "&", "<", `"`, `\`} {
		name := strings.Repeat(r, maxNameRunes)
		c := cursor(name, uuid.New())
		if len(c) > limit {
			t.Errorf("cursor for 200 × %q is %d characters, over the spec's %d", r, len(c), limit)
		}
		if _, after, err := (Page{Cursor: c}).resolve(); err != nil || after.Name != name {
			t.Errorf("cursor for 200 × %q does not round-trip: %v", r, err)
		}
	}
}

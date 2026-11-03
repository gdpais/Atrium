package inventory

import (
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
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

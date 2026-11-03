package inventory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Page sizes.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Page asks for one page of a list.
type Page struct {
	// Limit is the most items the page holds: DefaultLimit when 0.
	Limit int
	// Cursor is where the page starts, the NextCursor of the page before;
	// "" for the first page.
	Cursor string
}

// position is where a page starts: after the item with this name and ID,
// in the lists' order (name, then ID). Keyset, not offset: an item added
// or removed while a client pages neither repeats nor skips another.
type position struct {
	Name string    `json:"n"`
	ID   uuid.UUID `json:"i"`
}

// resolve returns the page's limit and where it starts, nil for the first
// page.
func (p Page) resolve() (int, *position, error) {
	limit := p.Limit
	switch {
	case limit == 0:
		limit = DefaultLimit
	case limit < 1 || limit > MaxLimit:
		return 0, nil, invalid("limit", "want 1 to %d", MaxLimit)
	}
	if p.Cursor == "" {
		return limit, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	var after position
	if err == nil {
		err = json.Unmarshal(raw, &after)
	}
	// The name is one a list returned: a crafted one that PostgreSQL would
	// refuse, such as one holding a NUL, is the client's error, not a 500.
	if err != nil || after.ID == uuid.Nil || !utf8.ValidString(after.Name) ||
		utf8.RuneCountInString(after.Name) > maxNameRunes || strings.ContainsFunc(after.Name, unicode.IsControl) {
		return 0, nil, invalid("cursor", "not one this API returned; start again without it")
	}
	return limit, &after, nil
}

// cursor encodes the position after an item. HTML characters are left as
// they are, so that a cursor stays within the spec's maxLength whatever
// the name.
func cursor(name string, id uuid.UUID) string {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(position{Name: name, ID: id}) // cannot fail
	return base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(raw.Bytes(), []byte("\n")))
}

// page cuts rows, fetched with limit+1, to limit, and returns the cursor of
// the next page when there is one.
func page[T any](rows []T, limit int, at func(T) (string, uuid.UUID)) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	return rows, cursor(at(rows[limit-1]))
}

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"atrium/apps/api/internal/openapi"
)

// requestError is a request the operation does not accept, found after the
// generated code decoded it; Register answers it with 400.
type requestError struct{ detail string }

func (e *requestError) Error() string { return e.detail }

// keepBody reads the request body, which the guard bounds, and keeps it for
// closedBodies, which needs it again after the generated code has decoded
// it. It runs after the guard, so a refused request's body is never read.
func (s *Server) keepBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeProblem(w, http.StatusBadRequest, fmt.Sprintf("the body is larger than %d bytes", tooLarge.Limit))
			return
		case err != nil:
			writeProblem(w, http.StatusBadRequest, "the body could not be read")
			return
		}
		requestFrom(r.Context()).body = raw
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, r)
	})
}

// closedBodies refuses a JSON body with a member its schema does not list,
// or with anything after the object: the spec's request schemas are closed
// (additionalProperties: false), and the generated code ignores unknown
// members. A client that sends a field it cannot set, such as an
// application's environment_id to a PATCH, is told so instead of seeing
// the request succeed without it.
func (s *Server) closedBodies(next openapi.StrictHandlerFunc, _ string) openapi.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if err := decodeClosed(request, requestFrom(ctx).body); err != nil {
			return nil, &requestError{detail: err.Error()}
		}
		return next(ctx, w, r, request)
	}
}

// decodeClosed checks raw against the type of request's Body: one JSON
// object whose members are all, exactly as spelt, fields of that type.
// encoding/json matches member names without regard to case, so a
// DisallowUnknownFields decode would let "Name" stand for "name".
// Operations without a body pass.
func decodeClosed(request any, raw []byte) error {
	v := reflect.ValueOf(request)
	if v.Kind() != reflect.Struct {
		return nil
	}
	body := v.FieldByName("Body")
	if !body.IsValid() || body.Kind() != reflect.Pointer || body.IsNil() || body.Type().Elem().Kind() != reflect.Struct {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return fmt.Errorf("the body: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the body holds more than one JSON value")
	}
	known := members(body.Type().Elem())
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if !known[name] {
			return fmt.Errorf("the body: unknown field %q; the operation's schema lists the members it accepts", name)
		}
	}
	return nil
}

// members returns the JSON member names of a generated body type's fields.
func members(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
		case "":
			out[f.Name] = true
		default:
			out[name] = true
		}
	}
	return out
}

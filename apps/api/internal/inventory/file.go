package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"heartbeat/apps/api/internal/probedefs"
	"heartbeat/apps/api/internal/store"
	configschema "heartbeat/packages/config-schema"
	"heartbeat/services/db-collector/probecatalog"
)

// FileVersion is the only inventory file format so far.
const FileVersion = 1

// MaxFileBytes bounds an inventory file.
const MaxFileBytes = 4 << 20

// File is one environment's inventory as code (ADR 0021): the file
// `api inventory check` checks and the API applies. ParseFile returns it
// normalized, as the API would store it: names trimmed, defaults filled in,
// hosts in lower case.
type File struct {
	Version         int                  `json:"version"`
	Environment     FileEnvironment      `json:"environment"`
	Applications    []FileApplication    `json:"applications,omitempty"`
	DatabaseTargets []FileDatabaseTarget `json:"database_targets,omitempty"`
}

// FileEnvironment is the file's environment.
type FileEnvironment struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Status      string            `json:"status,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// FileApplication is an application of the environment, with its
// components and telemetry sources.
type FileApplication struct {
	Name             string          `json:"name"`
	Platform         string          `json:"platform"`
	OwnerTeam        string          `json:"owner_team,omitempty"`
	Criticality      string          `json:"criticality,omitempty"`
	Status           string          `json:"status,omitempty"`
	Components       []FileComponent `json:"components,omitempty"`
	TelemetrySources []FileSource    `json:"telemetry_sources,omitempty"`
}

// FileComponent is a component of an application.
type FileComponent struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	RuntimeName string            `json:"runtime_name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// FileSource is a telemetry source of an application.
type FileSource struct {
	Type       string         `json:"type"`
	IngestMode string         `json:"ingest_mode"`
	Status     string         `json:"status,omitempty"`
	Config     map[string]any `json:"config"`
}

// FileDatabaseTarget is a database target of the environment, with the
// probes the collector runs against it.
type FileDatabaseTarget struct {
	Name          string            `json:"name"`
	Engine        string            `json:"engine,omitempty"`
	Host          string            `json:"host"`
	Port          int               `json:"port,omitempty"`
	DatabaseName  string            `json:"database_name,omitempty"`
	CredentialRef string            `json:"credential_ref"`
	Status        string            `json:"status,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Probes        []FileProbe       `json:"probes,omitempty"`
}

// FileProbe is a built-in probe a target runs: a probe assignment, approved
// by the review of the file's pull request.
type FileProbe struct {
	Name            string `json:"name"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
	MaxRows         int    `json:"max_rows,omitempty"`
	// Enabled is nil for true.
	Enabled *bool `json:"enabled,omitempty"`
}

// On reports whether the probe is enabled.
func (p FileProbe) On() bool { return p.Enabled == nil || *p.Enabled }

// FileError is an inventory file that cannot be applied: every problem
// found, each with where it is in the file.
type FileError struct{ Problems []string }

func (e *FileError) Error() string {
	if len(e.Problems) == 1 {
		return "the inventory file has a problem: " + e.Problems[0]
	}
	return fmt.Sprintf("the inventory file has %d problems:\n- %s", len(e.Problems), strings.Join(e.Problems, "\n- "))
}

// inventorySchema is the inventory file's schema, compiled once.
var inventorySchema = func() *jsonschema.Schema {
	schema, err := configschema.InventorySchema()
	if err != nil {
		panic(err) // embedded, and tested
	}
	return schema
}()

// LoadFile reads the inventory file at path and checks it as ParseFile
// does.
func LoadFile(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > MaxFileBytes {
		return File{}, &FileError{Problems: []string{fmt.Sprintf("larger than %d bytes", MaxFileBytes)}}
	}
	return ParseFile(data)
}

// ParseFile reads one YAML document as an inventory file and checks it with
// the schema (packages/config-schema/src/inventory.schema.json) and with the
// rules the API applies to the same entities through its endpoints, which
// it needs no database for: each field, the names unique where the tables
// make them unique, a source type's config, and each probe's policy against
// the collector's catalog. It returns the file normalized, or a *FileError
// with every problem it found.
func ParseFile(data []byte) (File, error) {
	doc, err := decodeYAML(data)
	if err != nil {
		return File{}, &FileError{Problems: []string{err.Error()}}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return File{}, &FileError{Problems: []string{fmt.Sprintf("not representable as JSON: %v", err)}}
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return File{}, &FileError{Problems: []string{fmt.Sprintf("not representable as JSON: %v", err)}}
	}
	if err := inventorySchema.Validate(instance); err != nil {
		return File{}, &FileError{Problems: schemaErrors(err)}
	}
	var file File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		// The schema is closed, so this is a schema and struct mismatch.
		return File{}, fmt.Errorf("decode the inventory file: %w", err)
	}
	c := &fileChecker{}
	c.check(&file)
	if len(c.problems) > 0 {
		return File{}, &FileError{Problems: c.problems}
	}
	return file, nil
}

// decodeYAML decodes exactly one YAML document into JSON-shaped values:
// objects with string keys, arrays, strings, numbers, booleans and null.
// yaml.v3 refuses a key repeated in one mapping.
func decodeYAML(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty: want a YAML document with version and environment")
		}
		return nil, fmt.Errorf("not valid YAML: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document: want one environment per file")
	}
	return jsonShaped(doc, "")
}

func jsonShaped(v any, at string) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			shaped, err := jsonShaped(item, at+"/"+k)
			if err != nil {
				return nil, err
			}
			v[k] = shaped
		}
		return v, nil
	case map[any]any:
		return nil, fmt.Errorf("at %q: a key that is not a string; quote it", at)
	case []any:
		for i, item := range v {
			shaped, err := jsonShaped(item, fmt.Sprintf("%s/%d", at, i))
			if err != nil {
				return nil, err
			}
			v[i] = shaped
		}
		return v, nil
	case nil, string, bool, int, int64, uint64, float64:
		return v, nil
	default:
		// A timestamp, for one: the schema wants a string, so quote it.
		return nil, fmt.Errorf("at %q: a YAML %T, not a string, number or boolean; quote it", at, v)
	}
}

// printer writes the schema's findings in English.
var printer = message.NewPrinter(language.English)

// schemaErrors lists the schema's findings, one per leaf, each with where
// it is in the file.
func schemaErrors(err error) []string {
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			reason := e.ErrorKind.LocalizedString(printer)
			if n := len(e.InstanceLocation); n > 0 && e.InstanceLocation[n-1] == "credential_ref" {
				// The schema's message quotes the value, which may be a
				// credential pasted in by mistake: keep it out of CI logs.
				reason = "want kv/, secret/ or env/ and a name of lower-case letters, digits and '-', such as env/finance-prod; the value is not shown"
			}
			out = append(out, fmt.Sprintf("%s: %s", filePath(e.InstanceLocation), reason))
			return
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(verr)
	slices.Sort(out)
	return slices.Compact(out)
}

// filePath writes an instance location as the file's own path, such as
// applications[0].components[1].name.
func filePath(location []string) string {
	if len(location) == 0 {
		return "(file)"
	}
	var b strings.Builder
	for _, part := range location {
		if part != "" && strings.Trim(part, "0123456789") == "" {
			b.WriteString("[" + part + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(part)
	}
	return b.String()
}

// placeholderID stands in for the IDs the API's checks require of an
// owner, which a file names by position instead.
var placeholderID = uuid.UUID{0: 1}

// builtinProbes are the collector's built-in probes, by engine and name.
var builtinProbes = func() map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, p := range probecatalog.Builtins() {
		out[[2]string{p.Engine, p.Name}] = true
	}
	return out
}()

type fileChecker struct{ problems []string }

func (c *fileChecker) add(at, format string, args ...any) {
	c.problems = append(c.problems, at+": "+fmt.Sprintf(format, args...))
}

// fail records err, an *InvalidError of the API's checks, at the field the
// file names; renames maps the API's field names to the file's.
func (c *fileChecker) fail(at string, err error, renames map[string]string) {
	var inv *InvalidError
	if !errors.As(err, &inv) {
		c.add(at, "%v", err)
		return
	}
	field := inv.Field
	if renamed, ok := renames[field]; ok {
		field = renamed
	}
	c.add(at+"."+field, "%s", inv.Reason)
}

func (c *fileChecker) check(f *File) {
	c.environment(&f.Environment)
	names := map[string]int{}
	// applicationNames maps a source's application_name to the application
	// that claims it: within an environment, a name is one application's.
	applicationNames := map[string]int{}
	for i := range f.Applications {
		at := fmt.Sprintf("applications[%d]", i)
		a := &f.Applications[i]
		if !c.application(at, a) {
			continue
		}
		if j, ok := names[a.Name]; ok {
			c.add(at+".name", "%q is applications[%d]'s too: an application's name is unique in its environment", a.Name, j)
		} else {
			names[a.Name] = i
		}
		c.components(at, a.Components)
		c.sources(at, i, a.TelemetrySources, applicationNames)
	}
	c.targets(f.DatabaseTargets)
}

func (c *fileChecker) environment(e *FileEnvironment) {
	p, err := newEnvironmentParams(NewEnvironment{Name: e.Name, Slug: e.Slug, Type: e.Type, Description: e.Description, Status: e.Status, Labels: e.Labels})
	if err != nil {
		c.fail("environment", err, nil)
		return
	}
	e.Name, e.Description, e.Status = p.Name, p.Description, p.Status
}

func (c *fileChecker) application(at string, a *FileApplication) bool {
	p, err := newApplicationParams(NewApplication{EnvironmentID: placeholderID, Name: a.Name, Platform: a.Platform, OwnerTeam: a.OwnerTeam, Criticality: a.Criticality, Status: a.Status})
	if err != nil {
		c.fail(at, err, nil)
		return false
	}
	a.Name, a.OwnerTeam, a.Criticality, a.Status = p.Name, p.OwnerTeam, p.Criticality, p.Status
	return true
}

func (c *fileChecker) components(app string, components []FileComponent) {
	seen := map[[2]string]int{}
	for i := range components {
		at := fmt.Sprintf("%s.components[%d]", app, i)
		k := &components[i]
		p, err := newComponentParams(NewComponent{ApplicationID: placeholderID, Name: k.Name, ComponentType: k.Type, RuntimeName: k.RuntimeName, Labels: k.Labels})
		if err != nil {
			c.fail(at, err, map[string]string{"component_type": "type"})
			continue
		}
		k.Name, k.RuntimeName = p.Name, p.RuntimeName
		key := [2]string{k.Name, k.Type}
		if j, ok := seen[key]; ok {
			c.add(at, "%s component %q is %s.components[%d] too: name and type are unique in an application", k.Type, k.Name, app, j)
			continue
		}
		seen[key] = i
	}
}

func (c *fileChecker) sources(app string, appIndex int, sources []FileSource, applicationNames map[string]int) {
	seen := map[[3]string]int{}
	for i := range sources {
		at := fmt.Sprintf("%s.telemetry_sources[%d]", app, i)
		s := &sources[i]
		p, name, err := newSourceParams(NewSource{ApplicationID: placeholderID, SourceType: s.Type, IngestMode: s.IngestMode, Status: s.Status, Config: s.Config})
		if err != nil {
			c.fail(at, err, map[string]string{"source_type": "type"})
			continue
		}
		s.Status = p.Status
		if other, ok := applicationNames[name]; ok && other != appIndex {
			c.add(at+".config.application_name", "%q is applications[%d]'s: within an environment, an application_name belongs to one application", name, other)
			continue
		}
		applicationNames[name] = appIndex
		key := [3]string{s.Type, s.IngestMode, name}
		if j, ok := seen[key]; ok {
			c.add(at, "the same %s %s source for %q as %s.telemetry_sources[%d]", s.Type, s.IngestMode, name, app, j)
			continue
		}
		seen[key] = i
	}
}

func (c *fileChecker) targets(targets []FileDatabaseTarget) {
	names := map[string]int{}
	identities := map[[4]string]int{}
	for i := range targets {
		at := fmt.Sprintf("database_targets[%d]", i)
		t := &targets[i]
		norm, err := newDatabaseTarget(NewDatabaseTarget{EnvironmentID: placeholderID, Engine: t.Engine, Name: t.Name, Host: t.Host, Port: t.Port, DatabaseName: t.DatabaseName, CredentialRef: t.CredentialRef, Status: t.Status, Labels: t.Labels})
		if err != nil {
			c.fail(at, err, nil)
			continue
		}
		t.Engine, t.Host, t.Port, t.DatabaseName, t.Status = norm.Engine, norm.Host, norm.Port, norm.DatabaseName, norm.Status
		if j, ok := names[t.Name]; ok {
			c.add(at+".name", "%q is database_targets[%d]'s too: a target's name is unique in its environment", t.Name, j)
		} else {
			names[t.Name] = i
		}
		identity := [4]string{t.Engine, t.Host, fmt.Sprint(t.Port), strings.ToLower(t.DatabaseName)}
		if j, ok := identities[identity]; ok {
			c.add(at, "the same %s server, port and database as database_targets[%d]: one database is one target", t.Engine, j)
		} else {
			identities[identity] = i
		}
		c.probes(at, t)
	}
}

func (c *fileChecker) probes(target string, t *FileDatabaseTarget) {
	seen := map[string]int{}
	interval, intervalAt := 0, ""
	for i := range t.Probes {
		at := fmt.Sprintf("%s.probes[%d]", target, i)
		p := &t.Probes[i]
		switch {
		case strings.HasPrefix(p.Name, probedefs.CustomPrefix):
			c.add(at+".name", "%q: custom probe definitions are not in the inventory file yet; assign it through the API", p.Name)
			continue
		case !builtinProbes[[2]string{t.Engine, p.Name}]:
			c.add(at+".name", "%q is no built-in %s probe (services/db-collector/probecatalog)", p.Name, t.Engine)
			continue
		}
		if j, ok := seen[p.Name]; ok {
			c.add(at+".name", "%q is %s.probes[%d] too: a target runs each probe once", p.Name, target, j)
			continue
		}
		seen[p.Name] = i
		if p.IntervalSeconds == 0 {
			p.IntervalSeconds = probedefs.DefaultIntervalSeconds
		}
		// A built-in definition sets no timeout of its own.
		if err := checkPolicy(ProbeAssignment{IntervalSeconds: p.IntervalSeconds, TimeoutMS: p.TimeoutMS, MaxRows: p.MaxRows}, store.ProbeDefinition{}); err != nil {
			c.fail(at, err, nil)
			continue
		}
		if !p.On() {
			continue
		}
		// One collector scrapes a target, at one interval (ADR 0014 §2).
		if interval == 0 {
			interval, intervalAt = p.IntervalSeconds, at
		} else if p.IntervalSeconds != interval {
			c.add(at+".interval_seconds", "%ds, and %s runs every %ds: a target's enabled probes share one interval", p.IntervalSeconds, intervalAt, interval)
		}
	}
}

// Package scenario reads PatientZero's fault scenarios (ADR 0010): YAML files
// that name a fault from a registry, the steps it runs for, and the signals
// it must produce in Prometheus, Loki and Alertmanager. The format is
// documented in tools/patientzero/scenarios/README.md and described by
// tools/patientzero/scenarios/scenario.schema.json.
package scenario

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the scenario format version this package reads.
const Version = 1

// Limits that keep a scenario bounded. The schema repeats them.
const (
	MaxSteps        = 20
	MaxSignals      = 50
	MaxStepDuration = 24 * time.Hour
	MaxWithin       = 6 * time.Hour
)

// Load profiles a driver may run during a scenario: the k6 profiles of
// load/patientzero.js, or none.
var LoadProfiles = []string{"none", "smoke", "steady", "peak", "soak", "day"}

// namePattern is a scenario, step or signal name: lower case, digits and
// dashes, so it is safe in a file name, a URL path and a run id.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether s can name a scenario, step or signal.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// Scenario is one scenario file.
type Scenario struct {
	Version     int      `yaml:"version" json:"version"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Load        string   `yaml:"load" json:"load"`
	Steps       []Step   `yaml:"steps" json:"steps"`
	Signals     []Signal `yaml:"signals" json:"signals"`
}

// Step is a stretch of the run, with or without a fault.
type Step struct {
	Name     string     `yaml:"name" json:"name"`
	Duration Duration   `yaml:"duration" json:"duration"`
	Fault    *FaultSpec `yaml:"fault,omitempty" json:"fault,omitempty"`
}

// FaultSpec names a fault type from the registry and its parameters.
type FaultSpec struct {
	Type   string         `yaml:"type" json:"type"`
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
}

// Signal kinds: which source a signal is read from.
const (
	KindPromQL   = "promql"
	KindLogQL    = "logql"
	KindAlert    = "alert"
	KindDelivery = "delivery"
)

// MaxDeliveryReach is how far back from the end of its window a delivery
// check may count rows: the Ledger keeps its counts longer than this.
const MaxDeliveryReach = 6 * time.Hour

// LogTypes are the OutSystems log types a delivery check can count, as the
// collector's outsystems_log_type label names them.
var LogTypes = []string{"cyclic_job", "error", "extension", "general", "integration", "mobile_request", "request_event", "screen"}

// Delivery is a delivery check: Loki must hold exactly the log rows
// PatientZero committed, of each log type, with an Instant from the start
// of step FromStep to the end of step ToStep, except the rows a
// commit-delay fault held back longer than LostAfter, which must be
// missing: committed past the collector's lag, they are never read.  With
// LateRows, a Prometheus counter selector, the counter must also have grown
// by exactly the rows lost, of each log type (its log_type label), from the
// start of FromStep to the evaluation: the collector's recount counts them.
type Delivery struct {
	LogTypes  []string `yaml:"log_types" json:"log_types"`
	FromStep  string   `yaml:"from_step,omitempty" json:"from_step"`
	ToStep    string   `yaml:"to_step,omitempty" json:"to_step"`
	LostAfter Duration `yaml:"lost_after,omitempty" json:"lost_after,omitempty"`
	LateRows  string   `yaml:"late_rows,omitempty" json:"late_rows,omitempty"`
}

// counterSelector is a metric name, optionally with label matchers.
var counterSelector = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^{}]*\})?$`)

// Anchors: whether a signal's window opens at the start or the end of its
// step.
const (
	FromStart = "start"
	FromEnd   = "end"
)

// Modes: eventually passes the first time the condition holds within the
// window; always passes only if it holds at every evaluation until the
// window closes.
const (
	ModeEventually = "eventually"
	ModeAlways     = "always"
)

// Alert states a signal can expect.
const (
	StateFiring   = "firing"
	StateResolved = "resolved"
)

// Signal is one expected signal: a PromQL or LogQL expression with a
// condition, or an Alertmanager alert expected to fire or resolve, within a
// window that opens at the start or end of a step.
type Signal struct {
	Name      string            `yaml:"name" json:"name"`
	PromQL    string            `yaml:"promql,omitempty" json:"promql,omitempty"`
	LogQL     string            `yaml:"logql,omitempty" json:"logql,omitempty"`
	Alert     string            `yaml:"alert,omitempty" json:"alert,omitempty"`
	Delivery  *Delivery         `yaml:"delivery,omitempty" json:"delivery,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	State     string            `yaml:"state,omitempty" json:"state,omitempty"`
	Condition string            `yaml:"condition,omitempty" json:"condition,omitempty"`
	Step      string            `yaml:"step,omitempty" json:"step,omitempty"`
	From      string            `yaml:"from,omitempty" json:"from,omitempty"`
	Within    Duration          `yaml:"within" json:"within"`
	Mode      string            `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// Kind is the signal's source: promql, logql, alert or delivery.
func (s Signal) Kind() string {
	switch {
	case s.PromQL != "":
		return KindPromQL
	case s.LogQL != "":
		return KindLogQL
	case s.Delivery != nil:
		return KindDelivery
	default:
		return KindAlert
	}
}

// Query is the signal's expression, the alert name, or what a delivery
// check counts.
func (s Signal) Query() string {
	switch s.Kind() {
	case KindPromQL:
		return s.PromQL
	case KindLogQL:
		return s.LogQL
	case KindDelivery:
		d := s.Delivery
		q := fmt.Sprintf("%s rows from the start of %s to the end of %s", strings.Join(d.LogTypes, ", "), d.FromStep, d.ToStep)
		if d.LostAfter > 0 {
			q += fmt.Sprintf(", but those committed more than %s late", d.LostAfter)
		}
		if d.LateRows != "" {
			q += fmt.Sprintf(", which %s counts", d.LateRows)
		}
		return q
	default:
		return s.Alert
	}
}

// Duration is a time.Duration written as a Go duration string ("90s",
// "2m30s") in YAML and JSON.
type Duration time.Duration

// D is the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func parseDuration(s string) (Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 90s or 2m30s", s)
	}
	return Duration(v), nil
}

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag == "!!int" || n.Tag == "!!float" {
		return fmt.Errorf("line %d: a duration is a string such as 90s or 2m30s", n.Line)
	}
	v, err := parseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = v
	return nil
}

// MarshalJSON writes the duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a duration is a string such as 90s or 2m30s")
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Condition is a comparison of a query's value: > >= < <= == != against a
// number, or present / absent (the query returns data, or none).
type Condition struct {
	Op    string
	Value float64
}

var conditionOps = []string{">=", "<=", "==", "!=", ">", "<"}

// ParseCondition reads a condition such as "> 0.25", "== 0" or "absent".
func ParseCondition(s string) (Condition, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "present", "absent":
		return Condition{Op: s}, nil
	}
	for _, op := range conditionOps {
		if rest, ok := strings.CutPrefix(s, op); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return Condition{}, fmt.Errorf("condition %q: %q is not a number", s, strings.TrimSpace(rest))
			}
			return Condition{Op: op, Value: v}, nil
		}
	}
	return Condition{}, fmt.Errorf("condition %q must be present, absent, or one of > >= < <= == != and a number", s)
}

// Holds reports whether the condition holds for the values a query
// returned: every value for a comparison (and at least one), or whether
// there are any for present and absent.
func (c Condition) Holds(values []float64) bool {
	switch c.Op {
	case "present":
		return len(values) > 0
	case "absent":
		return len(values) == 0
	}
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		if !c.compare(v) {
			return false
		}
	}
	return true
}

func (c Condition) compare(v float64) bool {
	switch c.Op {
	case ">":
		return v > c.Value
	case ">=":
		return v >= c.Value
	case "<":
		return v < c.Value
	case "<=":
		return v <= c.Value
	case "==":
		return v == c.Value
	case "!=":
		return v != c.Value
	}
	return false
}

// Parse reads one scenario file strictly (unknown fields are errors), fills
// the defaults and validates it against the registry.
func Parse(data []byte, reg *Registry) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("a scenario file holds one YAML document")
	}
	s.defaults()
	if err := s.Validate(reg); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Scenario) defaults() {
	if s.Load == "" {
		s.Load = "steady"
	}
	anchor := ""
	for _, st := range s.Steps {
		if st.Fault != nil {
			anchor = st.Name
			break
		}
	}
	if anchor == "" && len(s.Steps) > 0 {
		anchor = s.Steps[0].Name
	}
	for i := range s.Signals {
		sig := &s.Signals[i]
		if sig.Step == "" {
			sig.Step = anchor
		}
		if d := sig.Delivery; d != nil && len(s.Steps) > 0 {
			if d.FromStep == "" {
				d.FromStep = s.Steps[0].Name
			}
			if d.ToStep == "" {
				d.ToStep = s.Steps[len(s.Steps)-1].Name
			}
		}
		if sig.From == "" {
			sig.From = FromStart
		}
		if sig.Mode == "" {
			sig.Mode = ModeEventually
		}
	}
}

// Validate checks the scenario: names, durations, that each step's fault
// type is in the registry with valid parameters, and that each signal reads
// one source with a condition (or an alert state) and anchors on a step.
func (s *Scenario) Validate(reg *Registry) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if s.Version != Version {
		add("version must be %d, not %d", Version, s.Version)
	}
	if !ValidName(s.Name) {
		add("name %q must be lower case letters, digits and dashes, at most 63 characters", s.Name)
	}
	if strings.TrimSpace(s.Description) == "" {
		add("description is required")
	}
	if !contains(LoadProfiles, s.Load) {
		add("load must be one of %s, not %q", strings.Join(LoadProfiles, ", "), s.Load)
	}
	if len(s.Steps) == 0 || len(s.Steps) > MaxSteps {
		add("steps must list 1 to %d steps", MaxSteps)
	}
	steps := map[string]bool{}
	for i, st := range s.Steps {
		where := fmt.Sprintf("steps[%d]", i)
		if !ValidName(st.Name) {
			add("%s: name %q must be lower case letters, digits and dashes", where, st.Name)
		} else if steps[st.Name] {
			add("%s: step name %q is used twice", where, st.Name)
		}
		steps[st.Name] = true
		if st.Duration.D() <= 0 || st.Duration.D() > MaxStepDuration {
			add("%s: duration must be more than 0 and at most %s", where, MaxStepDuration)
		}
		if st.Fault != nil {
			if _, err := reg.New(*st.Fault); err != nil {
				add("%s: fault: %w", where, err)
			}
		}
	}
	if len(s.Signals) > MaxSignals {
		add("signals must list at most %d signals", MaxSignals)
	}
	signals := map[string]bool{}
	for i, sig := range s.Signals {
		where := fmt.Sprintf("signals[%d]", i)
		if !ValidName(sig.Name) {
			add("%s: name %q must be lower case letters, digits and dashes", where, sig.Name)
		} else if signals[sig.Name] {
			add("%s: signal name %q is used twice", where, sig.Name)
		}
		signals[sig.Name] = true
		sources := 0
		for _, q := range []string{sig.PromQL, sig.LogQL, sig.Alert} {
			if strings.TrimSpace(q) != "" {
				sources++
			}
		}
		if sig.Delivery != nil {
			sources++
		}
		if sources != 1 {
			add("%s: give exactly one of promql, logql, alert or delivery", where)
		}
		switch sig.Kind() {
		case KindDelivery:
			s.validateDelivery(&sig, where, add)
		case KindAlert:
			if sig.State != StateFiring && sig.State != StateResolved {
				add("%s: an alert signal needs state firing or resolved", where)
			}
			if sig.Condition != "" {
				add("%s: an alert signal has a state, not a condition", where)
			}
			for k := range sig.Labels {
				if !labelName.MatchString(k) {
					add("%s: label %q is not a label name", where, k)
				}
			}
		default:
			if _, err := ParseCondition(sig.Condition); err != nil {
				add("%s: %w", where, err)
			}
			if sig.State != "" || len(sig.Labels) > 0 {
				add("%s: state and labels are for alert signals", where)
			}
		}
		if !steps[sig.Step] {
			add("%s: step %q is not a step of this scenario", where, sig.Step)
		}
		if sig.From != FromStart && sig.From != FromEnd {
			add("%s: from must be start or end, not %q", where, sig.From)
		}
		if sig.Mode != ModeEventually && sig.Mode != ModeAlways {
			add("%s: mode must be eventually or always, not %q", where, sig.Mode)
		}
		if sig.Within.D() <= 0 || sig.Within.D() > MaxWithin {
			add("%s: within must be more than 0 and at most %s", where, MaxWithin)
		}
	}
	return errors.Join(errs...)
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateDelivery checks a delivery signal: known log types, each once; its
// row window's steps, in order; and a window that opens once the rows'
// window has closed, so every row it counts has been written.
func (s *Scenario) validateDelivery(sig *Signal, where string, add func(string, ...any)) {
	d := sig.Delivery
	if len(d.LogTypes) == 0 {
		add("%s: delivery needs log_types", where)
	}
	seen := map[string]bool{}
	for _, t := range d.LogTypes {
		if !contains(LogTypes, t) {
			add("%s: log type %q is not one of %s", where, t, strings.Join(LogTypes, ", "))
		} else if seen[t] {
			add("%s: log type %q is listed twice", where, t)
		}
		seen[t] = true
	}
	from, okFrom := s.StepIndex(d.FromStep)
	to, okTo := s.StepIndex(d.ToStep)
	if !okFrom {
		add("%s: from_step %q is not a step of this scenario", where, d.FromStep)
	}
	if !okTo {
		add("%s: to_step %q is not a step of this scenario", where, d.ToStep)
	}
	if okFrom && okTo && from > to {
		add("%s: from_step %q comes after to_step %q", where, d.FromStep, d.ToStep)
	}
	if d.LostAfter < 0 || d.LostAfter.D() > MaxStepDuration {
		add("%s: lost_after must be a positive duration of at most %s", where, MaxStepDuration)
	}
	if d.LateRows != "" && d.LostAfter == 0 {
		add("%s: late_rows needs lost_after: it counts the rows lost", where)
	}
	if d.LateRows != "" && (!counterSelector.MatchString(d.LateRows) || strings.Contains(d.LateRows, "log_type")) {
		add("%s: late_rows must be a counter's name with optional label matchers, without log_type, such as atrium_outsystems_late_rows_total{collector=\"patientzero\"}", where)
	}
	if okTo {
		if end, _ := s.StepStart(d.ToStep); s.Anchor(*sig) < end+s.Steps[to].Duration.D() {
			add("%s: a delivery check's window opens at or after the end of its to_step %q", where, d.ToStep)
		}
	}
	if okFrom {
		if start, _ := s.StepStart(d.FromStep); s.Anchor(*sig)+sig.Within.D()-start > MaxDeliveryReach {
			add("%s: a delivery check counts rows at most %s back from the end of its window", where, MaxDeliveryReach)
		}
	}
	if sig.Mode == ModeAlways {
		add("%s: a delivery check waits for Loki to catch up: mode eventually, not always", where)
	}
	if sig.Condition != "" || sig.State != "" || len(sig.Labels) > 0 {
		add("%s: a delivery check has no condition, state or labels", where)
	}
}

// StepIndex is the index of the step called name.
func (s *Scenario) StepIndex(name string) (int, bool) {
	for i, st := range s.Steps {
		if st.Name == name {
			return i, true
		}
	}
	return 0, false
}

// StepStart is when step name starts, from the start of the run.
func (s *Scenario) StepStart(name string) (time.Duration, bool) {
	var at time.Duration
	for _, st := range s.Steps {
		if st.Name == name {
			return at, true
		}
		at += st.Duration.D()
	}
	return 0, false
}

// Anchor is when a signal's window opens, from the start of the run.
func (s *Scenario) Anchor(sig Signal) time.Duration {
	at, _ := s.StepStart(sig.Step)
	if sig.From == FromEnd {
		for _, st := range s.Steps {
			if st.Name == sig.Step {
				at += st.Duration.D()
			}
		}
	}
	return at
}

// StepsDuration is how long the steps take together.
func (s *Scenario) StepsDuration() time.Duration {
	var d time.Duration
	for _, st := range s.Steps {
		d += st.Duration.D()
	}
	return d
}

// MaxDuration is the longest a run can take: until the last step ends or
// the last signal's window closes, whichever is later.
func (s *Scenario) MaxDuration() time.Duration {
	d := s.StepsDuration()
	for _, sig := range s.Signals {
		if end := s.Anchor(sig) + sig.Within.D(); end > d {
			d = end
		}
	}
	return d
}

// FaultTypes are the fault types the scenario's steps use, sorted.
func (s *Scenario) FaultTypes() []string {
	seen := map[string]bool{}
	var out []string
	for _, st := range s.Steps {
		if st.Fault != nil && !seen[st.Fault.Type] {
			seen[st.Fault.Type] = true
			out = append(out, st.Fault.Type)
		}
	}
	sort.Strings(out)
	return out
}

// LoadDir reads every *.yaml file in dir of fsys. A file's name must be its
// scenario's name, so the file that defines a scenario is easy to find.
func LoadDir(fsys fs.FS, dir string, reg *Registry) ([]*Scenario, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []*Scenario
	var errs []error
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		s, err := Parse(data, reg)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if want := strings.TrimSuffix(e.Name(), ".yaml"); s.Name != want {
			errs = append(errs, fmt.Errorf("%s: name is %q; a scenario file is named after its scenario", e.Name(), s.Name))
			continue
		}
		out = append(out, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

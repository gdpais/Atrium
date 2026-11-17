// Package journal writes PatientZero's run journals: one JSONL file per
// scenario run, append-only, one event per line, each in a versioned
// envelope. A journal is the known answer later investigations and reports
// are checked against (ADR 0010), so its format is a contract: the events
// and their fields are documented in tools/patientzero/scenarios/README.md,
// and a change that renames or removes a field bumps Version.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is the journal format version, in every line's "v".
const Version = 1

// Event types.
const (
	RunStart   = "run_start"
	FaultStart = "fault_start"
	FaultEnd   = "fault_end"
	Traffic    = "traffic"
	Check      = "check"
	RunEnd     = "run_end"
)

// Ext is a journal file's extension.
const Ext = ".jsonl"

// runIDPattern is a run id: the UTC start time, the scenario name and four
// random hex digits. Only such names are ever opened, so a run id from a
// request cannot name another path.
var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9][a-z0-9-]{0,62}-[0-9a-f]{4}$`)

// ValidRunID reports whether id has the form of a run id.
func ValidRunID(id string) bool { return runIDPattern.MatchString(id) }

// Line is one journal line: the envelope and the event's data.
type Line struct {
	V     int             `json:"v"`
	Seq   int             `json:"seq"`
	Time  time.Time       `json:"time"`
	Run   string          `json:"run"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// Journal is one run's journal file, open for appending.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	run  string
	seq  int
	path string
	err  error
}

// Create creates the journal of run id in dir. It never overwrites a file.
func Create(dir, id string) (*Journal, error) {
	if !ValidRunID(id) {
		return nil, fmt.Errorf("journal: %q is not a run id", id)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	p := filepath.Join(dir, id+Ext)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return &Journal{f: f, run: id, path: p}, nil
}

// Path is the journal file's path.
func (j *Journal) Path() string { return j.path }

// Write appends one event at time at with data, a value that marshals to a
// JSON object. An event that does not encode (a NaN, say) is not written and
// its error returned; it takes no sequence number, so later events are still
// written. After a failed write to the file every later write fails with the
// same error, so a journal never has a gap in its sequence numbers.
func (j *Journal) Write(at time.Time, event string, data any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("journal: %s: %w", event, err)
	}
	line, err := json.Marshal(Line{V: Version, Seq: j.seq + 1, Time: at.UTC(), Run: j.run, Event: event, Data: raw})
	if err != nil {
		return fmt.Errorf("journal: %s: %w", event, err)
	}
	j.seq++
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		j.err = fmt.Errorf("journal: %w", err)
	}
	return j.err
}

// Close flushes the file to disk and closes it.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return j.err
	}
	err := errors.Join(j.f.Sync(), j.f.Close())
	j.f = nil
	if j.err == nil && err != nil {
		j.err = fmt.Errorf("journal: %w", err)
	}
	return j.err
}

// Info is one journal file on disk.
type Info struct {
	Run   string    `json:"run"`
	Bytes int64     `json:"bytes"`
	Mod   time.Time `json:"modified"`
}

// List lists the journals in dir, newest run first. A missing directory has
// none.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), Ext)
		if !ok || !e.Type().IsRegular() || !ValidRunID(id) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Run: id, Bytes: fi.Size(), Mod: fi.ModTime().UTC()})
	}
	// Run ids start with their UTC start time, so they sort by age.
	sort.Slice(out, func(i, k int) bool { return out[i].Run > out[k].Run })
	return out, nil
}

// Open opens the journal of run id in dir for reading.
func Open(dir, id string) (*os.File, error) {
	if !ValidRunID(id) {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(dir, id+Ext))
}

// Prune deletes all but the keep newest journals in dir, except the run
// keepRun.
func Prune(dir string, keep int, keepRun string) error {
	list, err := List(dir)
	if err != nil {
		return err
	}
	var errs []error
	kept := 0
	for _, in := range list {
		if in.Run == keepRun || kept < keep {
			kept++
			continue
		}
		if err := os.Remove(filepath.Join(dir, in.Run+Ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

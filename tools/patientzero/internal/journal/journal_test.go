package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

const run = "20261022T100000Z-checkout-errors-0a1b"

func TestWriteAppendsVersionedLines(t *testing.T) {
	dir := t.TempDir()
	j, err := Create(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Write(t0, RunStart, map[string]string{"scenario": "checkout-errors"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Write(t0.Add(time.Second).In(time.FixedZone("x", 3600)), RunEnd, map[string]string{"state": "passed"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, run+Ext))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode %v, want 0600", fi.Mode().Perm())
	}
	var lines []Line
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
	for i, l := range lines {
		if l.V != Version || l.Seq != i+1 || l.Run != run {
			t.Errorf("line %d envelope: %+v", i, l)
		}
	}
	if lines[0].Event != RunStart || string(lines[0].Data) != `{"scenario":"checkout-errors"}` {
		t.Errorf("line 1: %s %s", lines[0].Event, lines[0].Data)
	}
	if lines[1].Time.Location() != time.UTC || !lines[1].Time.Equal(t0.Add(time.Second)) {
		t.Errorf("line 2 time %v, want UTC", lines[1].Time)
	}
}

func TestCreateNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	j, err := Create(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if _, err := Create(dir, run); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Create: %v, want ErrExist", err)
	}
}

func TestRunIDsCannotNamePaths(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../etc/passwd", "x", run + "/..", "20261022T100000Z-a-0a1", "20261022T100000Z-A-0a1b"} {
		if _, err := Create(dir, id); err == nil {
			t.Errorf("Create accepted %q", id)
		}
		if _, err := Open(dir, id); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want ErrNotExist", id, err)
		}
	}
}

func TestListAndPrune(t *testing.T) {
	dir := t.TempDir()
	ids := []string{
		"20261020T100000Z-a-0001",
		"20261021T100000Z-a-0002",
		"20261022T100000Z-a-0003",
		"20261023T100000Z-a-0004",
	}
	for _, id := range ids {
		j, err := Create(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		j.Close()
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir)
	if err != nil || len(list) != 4 || list[0].Run != ids[3] || list[3].Run != ids[0] {
		t.Fatalf("List = %v, %v; want newest first, journals only", list, err)
	}
	if err := Prune(dir, 2, ids[3]); err != nil {
		t.Fatal(err)
	}
	list, _ = List(dir)
	if len(list) != 2 || list[0].Run != ids[3] || list[1].Run != ids[2] {
		t.Fatalf("after Prune: %v, want the two newest", list)
	}
	if none, err := List(filepath.Join(dir, "missing")); err != nil || none != nil {
		t.Fatalf("missing dir: %v, %v", none, err)
	}
}

// An event that does not encode is not written and takes no sequence
// number; the events after it still are.
func TestUnencodableEventDoesNotBreakTheJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := Create(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Write(t0, Check, map[string]any{"values": []float64{math.NaN()}}); err == nil {
		t.Fatal("NaN check: no error")
	}
	if err := j.Write(t0, RunEnd, map[string]string{"state": "error"}); err != nil {
		t.Fatalf("run_end after a NaN: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, run+Ext))
	if err != nil {
		t.Fatal(err)
	}
	var l Line
	if err := json.Unmarshal(b, &l); err != nil || l.Seq != 1 || l.Event != RunEnd {
		t.Fatalf("journal %q (%v), want one run_end line with seq 1", b, err)
	}
}

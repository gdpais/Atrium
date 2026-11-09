package oslog

import (
	"testing"
	"time"

	"atrium/tools/patientzero/internal/scenario"
)

// A delivery check may count rows as far back as scenario.MaxDeliveryReach;
// the Ledger must still have them.
func TestLedgerKeepsWhatADeliveryCheckReaches(t *testing.T) {
	if LedgerRetention <= scenario.MaxDeliveryReach {
		t.Fatalf("LedgerRetention %s, want more than scenario.MaxDeliveryReach %s", LedgerRetention, scenario.MaxDeliveryReach)
	}
}

func screenAt(t time.Time) Row { return Row{Type: "screen", Columns: map[string]any{"Instant": t}} }

// A row belongs to the second (s-1, s] its stored Instant falls in, as a
// Loki range (from, to] counts it; the stored Instant is cut to the
// millisecond, so a time just past a second, in nanoseconds, still counts
// in that second.
func TestLedgerCountsByStoredSecond(t *testing.T) {
	base := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	l := Ledger{Now: func() time.Time { return base }}
	l.Add([]Row{
		screenAt(base), // (09:59:59, 10:00:00]
		screenAt(base.Add(400 * time.Microsecond)),  // stored as 10:00:00.000
		screenAt(base.Add(time.Millisecond)),        // (10:00:00, 10:00:01]
		screenAt(base.Add(time.Second)),             // (10:00:00, 10:00:01]
		screenAt(base.Add(1500 * time.Millisecond)), // (10:00:01, 10:00:02]
		{Type: "general", Columns: map[string]any{"Instant": base.Add(time.Second)}},
		{Type: "screen", Columns: map[string]any{"Screen": "no instant"}},
	}, 0)
	for _, c := range []struct {
		from, to time.Time
		want     int64
	}{
		{base.Add(-time.Second), base, 2},
		{base, base.Add(time.Second), 2},
		{base, base.Add(2 * time.Second), 3},
		{base.Add(-time.Minute), base.Add(time.Minute), 5},
		// Bounds are cut to the second.
		{base.Add(900 * time.Millisecond), base.Add(1900 * time.Millisecond), 2},
	} {
		if got := l.Count("screen", c.from, c.to, 0); got.Rows != c.want {
			t.Errorf("screen rows in (%s, %s]: %d, want %d", c.from.Format("15:04:05.000"), c.to.Format("15:04:05.000"), got.Rows, c.want)
		}
	}
	if got := l.Count("general", base, base.Add(time.Second), 0); got.Rows != 1 {
		t.Errorf("general rows: %d, want 1", got.Rows)
	}
}

// Held rows are counted by how long they were held: a check asks for those
// held longer than its lost_after.
func TestLedgerCountsHeldRowsByDelay(t *testing.T) {
	base := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	l := Ledger{Now: func() time.Time { return base }}
	l.Add([]Row{screenAt(base.Add(time.Second))}, 0)
	l.Add([]Row{screenAt(base.Add(time.Second)), screenAt(base.Add(2 * time.Second))}, 10*time.Second)
	l.Add([]Row{screenAt(base.Add(3 * time.Second))}, 90*time.Second)
	for heldOver, want := range map[time.Duration]int64{0: 3, 30 * time.Second: 1, 90 * time.Second: 0} {
		got := l.Count("screen", base, base.Add(time.Minute), heldOver)
		if got.Rows != 4 || got.Held != want {
			t.Errorf("held over %s: %+v, want 4 rows, %d held", heldOver, got, want)
		}
	}
}

func TestLedgerDropsOldCountsAndNilCountsNothing(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	l := &Ledger{Now: func() time.Time { return now }}
	old := now.Add(-LedgerRetention - time.Hour)
	l.Add([]Row{screenAt(old)}, 0)
	now = now.Add(2 * time.Minute)
	l.Add([]Row{screenAt(now)}, 0)
	if got := l.Count("screen", old.Add(-time.Minute), now, 0); got.Rows != 1 {
		t.Fatalf("rows %d: the one past the retention should be gone", got.Rows)
	}
	var none *Ledger
	none.Add([]Row{screenAt(now)}, 0)
	if got := none.Count("screen", old, now, 0); got.Rows != 0 {
		t.Fatal("a nil ledger counted")
	}
}

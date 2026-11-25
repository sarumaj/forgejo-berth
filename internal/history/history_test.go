// SPDX-License-Identifier: GPL-3.0-or-later

package history

import (
	"errors"
	"path/filepath"
	"testing"
)

const (
	day = 86400.0
	now = 1_000_000_000.0
)

type run struct {
	name     string
	peak     int
	daysAgo  float64
	repoName string
}

func family(name string) string {
	// The broker's family rule, for the test's few names.
	switch name {
	case "server (32)", "server (35)", "server (36)":
		return "server"
	}
	return name
}

func TestEstimate(t *testing.T) {
	type query struct{ repo, name string }
	for _, tc := range []struct {
		title      string
		runs       []run
		ask        query
		wantPeak   int
		wantSource string
		wantFound  bool
	}{
		{"the largest of the last runs",
			[]run{{"test", 1000, 40, ""}, {"test", 1500, 39, ""}, {"test", 1200, 38, ""}},
			query{"alice/app", "test"}, 1500, "job", true},
		{"a spike within the spike days is kept beyond the last runs",
			append([]run{{"test", 4000, 20, ""}}, series("test", 1000, 8)...),
			query{"alice/app", "test"}, 4000, "job", true},
		{"an old spike fades once newer runs replace it",
			append([]run{{"test", 4000, 40, ""}}, series("test", 1000, 5)...),
			query{"alice/app", "test"}, 1000, "job", true},
		{"a new matrix entry learns from its family",
			[]run{{"server (32)", 2000, 1, ""}, {"server (35)", 2600, 1, ""}},
			query{"alice/app", "server (36)"}, 2600, "family", true},
		{"a new job of a known repository learns from it",
			[]run{{"lint", 800, 1, ""}, {"build", 3100, 1, ""}},
			query{"alice/app", "e2e"}, 3100, "repo", true},
		{"a new repository is unknown",
			[]run{{"test", 800, 1, ""}},
			query{"alice/new", "test"}, 0, "none", false},
	} {
		t.Run(tc.title, func(t *testing.T) {
			h := open(t)
			for _, r := range tc.runs {
				repo := r.repoName
				if repo == "" {
					repo = "alice/app"
				}
				record(t, h, Run{Repo: repo, Name: r.name, Family: family(r.name), At: now - r.daysAgo*day,
					PeakMiB: r.peak, Seconds: 60, Host: "ci-1"})
			}
			peak, source, found, err := h.Estimate(tc.ask.repo, tc.ask.name, family(tc.ask.name), now-30*day, 5)
			if err != nil {
				t.Fatal(err)
			}
			if peak != tc.wantPeak || source != tc.wantSource || found != tc.wantFound {
				t.Fatalf("Estimate = %d, %s, %v; want %d, %s, %v", peak, source, found,
					tc.wantPeak, tc.wantSource, tc.wantFound)
			}
		})
	}
}

// series is n runs of the job with the peak, the last ones within 10 days.
func series(name string, peak, n int) []run {
	var out []run
	for i := range n {
		out = append(out, run{name, peak, float64(10 - i), ""})
	}
	return out
}

func TestPruneForgetsRunsPastTheRetention(t *testing.T) {
	h := open(t)
	record(t, h, Run{Repo: "alice/app", Name: "test", Family: "test", At: now - 200*day, PeakMiB: 800})
	record(t, h, Run{Repo: "alice/app", Name: "test", Family: "test", At: now - day, PeakMiB: 900})
	if n, err := h.Prune(now - 180*day); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want 1", n, err)
	}
	if n, err := h.Known(); err != nil || n != 1 {
		t.Fatalf("Known = %d, %v; want 1", n, err)
	}
}

func TestAssignmentsAreKeptAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.sqlite")
	h, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Hold("a-1", []byte(`{"id":"a-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.Hold("a-2", []byte(`{"id":"a-2"}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.Drop("a-2"); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	ro, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	held, err := ro.Held()
	if err != nil || len(held) != 1 || string(held["a-1"]) != `{"id":"a-1"}` {
		t.Fatalf("Held = %v, %v", held, err)
	}
	if err := ro.Record(Run{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a read-only history recorded: %v", err)
	}
}

func TestBackoffsAreKeptAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.sqlite")
	h, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []int64{7, 8} {
		if err := h.KeepBackoff(job, []byte(`{"unused":3}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.ForgetBackoff(8); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	h, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	kept, err := h.Backoffs()
	if err != nil || len(kept) != 1 || string(kept[7]) != `{"unused":3}` {
		t.Fatalf("Backoffs = %v, %v", kept, err)
	}
}

func TestAnOlderDatabaseGetsTheBackoffTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as it was before the back-off was kept.
	if _, err := old.db.Exec("DROP TABLE backoffs"); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	h, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.KeepBackoff(7, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if kept, err := h.Backoffs(); err != nil || len(kept) != 1 {
		t.Fatalf("Backoffs = %v, %v", kept, err)
	}
}

func TestReadOnlyNeedsAnExistingDatabase(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing.sqlite"), true); err == nil {
		t.Fatal("opened a missing database read-only")
	}
}

func open(t *testing.T) *History {
	t.Helper()
	h, err := Open(filepath.Join(t.TempDir(), "history.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func record(t *testing.T, h *History, r Run) {
	t.Helper()
	if err := h.Record(r); err != nil {
		t.Fatal(err)
	}
}

func TestCPUIsRecordedAndEstimated(t *testing.T) {
	h := open(t)
	record(t, h, Run{Repo: "alice/app", Name: "e2e", Family: "e2e", At: now - day, PeakMiB: 900, CPUCores: 3.2, CPUPressure: 0.4})
	// A run without CPU figures does not push a measured one out of the last runs.
	for i := range 6 {
		record(t, h, Run{Repo: "alice/app", Name: "e2e", Family: "e2e", At: now - 40*day + float64(i), PeakMiB: 800})
	}
	cores, source, found, err := h.EstimateCores("alice/app", "lint", "lint", now-30*day, 5)
	if err != nil || !found || source != "repo" || cores != 3.2 {
		t.Fatalf("EstimateCores = %v %s %v %v", cores, source, found, err)
	}
}

func TestAnOlderDatabaseGetsTheCPUColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as it was before CPU was recorded.
	for _, stmt := range []string{"DROP TABLE runs", "CREATE TABLE runs (repo TEXT NOT NULL, name TEXT NOT NULL," +
		" family TEXT NOT NULL, at REAL NOT NULL, peak_mib INTEGER NOT NULL, reserved_mib INTEGER NOT NULL," +
		" seconds REAL NOT NULL, host TEXT NOT NULL)", "INSERT INTO runs VALUES ('a/b', 'x', 'x', 1, 100, 0, 1, 'h')"} {
		if _, err := old.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close()
	ro, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := ro.EstimateCores("a/b", "x", "x", 0, 5); err != nil || found {
		t.Fatalf("a read-only older database: %v %v", found, err)
	}
	if peak, _, found, _ := ro.Estimate("a/b", "x", "x", 0, 5); !found || peak != 100 {
		t.Fatal("its memory history is lost")
	}
	_ = ro.Close()
	h, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	record(t, h, Run{Repo: "a/b", Name: "x", Family: "x", At: 2, PeakMiB: 1, CPUCores: 1.5})
	if cores, _, found, _ := h.EstimateCores("a/b", "x", "x", 0, 5); !found || cores != 1.5 {
		t.Fatal("the migrated database does not keep CPU figures")
	}
}

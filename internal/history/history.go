// SPDX-License-Identifier: GPL-3.0-or-later

// Package history keeps, in SQLite, the memory every finished job needed and the
// assignments the broker handed out, so that a restart keeps both.
//
// The schema is part of forgejo-berth's interface (docs/design.md, Shadow mode): a shadow
// instance reads a live instance's database.
package history

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	// The pure-Go SQLite driver keeps the binary static.
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (repo TEXT NOT NULL, name TEXT NOT NULL, family TEXT NOT NULL,
  at REAL NOT NULL, peak_mib INTEGER NOT NULL, reserved_mib INTEGER NOT NULL,
  seconds REAL NOT NULL, host TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS runs_job ON runs (repo, name, at);
CREATE INDEX IF NOT EXISTS runs_family ON runs (repo, family, at);
CREATE TABLE IF NOT EXISTS assignments (id TEXT PRIMARY KEY, data TEXT NOT NULL);
`

// cpuColumns are added to a database made before forgejo-berth recorded CPU use; a run
// without them (NULL) has no CPU figures.
var cpuColumns = []string{"cpu_cores", "cpu_pressure"}

// ErrReadOnly is returned by the writes of a history opened read-only.
var ErrReadOnly = errors.New("the history is read-only")

// History is the database.
type History struct {
	db       *sql.DB
	readOnly bool
	// cpu tells whether the runs table has the CPU columns (a read-only older database may
	// lack them).
	cpu bool
}

// Run is one finished job.
type Run struct {
	Repo, Name, Family string
	// At is when it was learned (Unix seconds).
	At          float64
	PeakMiB     int
	ReservedMiB int
	Seconds     float64
	Host        string
	// CPUCores is the job's average CPU use (cores: usage over its run time), CPUPressure
	// the share of its run time some of its tasks waited for a CPU; 0 when not measured.
	CPUCores    float64
	CPUPressure float64
}

// Open opens (and creates) the database at path; readOnly opens an existing one for reading
// only, beside the instance that writes it.
func Open(path string, readOnly bool) (*History, error) {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		query.Set("mode", "ro")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: path, RawQuery: query.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	h := &History{db: db, readOnly: readOnly}
	if !readOnly {
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("history %s: %w", path, err)
		}
	}
	columns, err := h.columns()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("history %s: %w", path, err)
	}
	h.cpu = true
	for _, c := range cpuColumns {
		if columns[c] {
			continue
		}
		if readOnly {
			h.cpu = false
			continue
		}
		if _, err := db.Exec("ALTER TABLE runs ADD COLUMN " + c + " REAL"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("history %s: %w", path, err)
		}
	}
	return h, nil
}

// columns returns the runs table's columns.
func (h *History) columns() (map[string]bool, error) {
	rows, err := h.db.Query("SELECT name FROM pragma_table_info('runs')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found[name] = true
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no runs table")
	}
	return found, rows.Err()
}

// Close closes the database.
func (h *History) Close() error {
	return h.db.Close()
}

// ReadOnly tells whether the history was opened for reading only.
func (h *History) ReadOnly() bool {
	return h.readOnly
}

// Record keeps one finished run (ReservedMiB 0: it had a host alone).
func (h *History) Record(r Run) error {
	if h.readOnly {
		return ErrReadOnly
	}
	var cores, pressure any
	if r.CPUCores > 0 {
		cores, pressure = r.CPUCores, r.CPUPressure
	}
	_, err := h.db.Exec("INSERT INTO runs (repo, name, family, at, peak_mib, reserved_mib, seconds, host,"+
		" cpu_cores, cpu_pressure) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.Repo, r.Name, r.Family, r.At, r.PeakMiB, r.ReservedMiB, r.Seconds, r.Host, cores, pressure)
	return err
}

// largest returns the largest peak of the runs matching where: those since, or the last
// runs of them, whichever reach further back.
func (h *History) largest(column, where string, values []any, since float64, runs int) (float64, bool, error) {
	// column and where are constants of this file, never input. Runs without the column
	// (no CPU measured) do not count among the last runs.
	where += " AND " + column + " IS NOT NULL"
	query := "SELECT MAX(v) FROM (SELECT " + column + " AS v FROM runs WHERE " + where + " AND at >= ?" +
		" UNION ALL SELECT v FROM (SELECT " + column + " AS v FROM runs WHERE " + where +
		" ORDER BY at DESC LIMIT ?))"
	args := append(append(append(append([]any{}, values...), since), values...), runs)
	var v sql.NullFloat64
	if err := h.db.QueryRow(query, args...).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Float64, v.Valid, nil
}

// lookups are the estimate's sources, in order.
func lookups(repo, name, family string) []struct {
	source, where string
	values        []any
} {
	return []struct {
		source, where string
		values        []any
	}{
		{"job", "repo = ? AND name = ?", []any{repo, name}},
		{"family", "repo = ? AND family = ?", []any{repo, family}},
		{"repo", "repo = ?", []any{repo}},
	}
}

// Estimate returns the largest peak to expect of a job and where it comes from: the job
// itself ("job"), its family ("family", the other entries of its matrix), its repository
// ("repo"), or none ("none", found false).
func (h *History) Estimate(repo, name, family string, since float64, runs int) (int, string, bool, error) {
	for _, q := range lookups(repo, name, family) {
		peak, found, err := h.largest("peak_mib", q.where, q.values, since, runs)
		if err != nil {
			return 0, "", false, err
		}
		if found {
			return int(peak), q.source, true, nil
		}
	}
	return 0, "none", false, nil
}

// EstimateCores returns the most cores to expect of a job, by the same rules as Estimate,
// from the runs whose CPU use was measured.
func (h *History) EstimateCores(repo, name, family string, since float64, runs int) (float64, string, bool, error) {
	if !h.cpu {
		return 0, "none", false, nil
	}
	for _, q := range lookups(repo, name, family) {
		cores, found, err := h.largest("cpu_cores", q.where, q.values, since, runs)
		if err != nil {
			return 0, "", false, err
		}
		if found {
			return cores, q.source, true, nil
		}
	}
	return 0, "none", false, nil
}

// Prune forgets the runs older than before; it returns how many.
func (h *History) Prune(before float64) (int64, error) {
	if h.readOnly {
		return 0, nil
	}
	result, err := h.db.Exec("DELETE FROM runs WHERE at < ?", before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Known returns how many jobs (repository and name) have a history.
func (h *History) Known() (int, error) {
	var n int
	err := h.db.QueryRow("SELECT COUNT(*) FROM (SELECT DISTINCT repo, name FROM runs)").Scan(&n)
	return n, err
}

// Hold keeps an assignment's data until it is dropped.
func (h *History) Hold(id string, data []byte) error {
	if h.readOnly {
		return ErrReadOnly
	}
	_, err := h.db.Exec("INSERT OR REPLACE INTO assignments VALUES (?, ?)", id, string(data))
	return err
}

// Drop forgets an assignment.
func (h *History) Drop(id string) error {
	if h.readOnly {
		return ErrReadOnly
	}
	_, err := h.db.Exec("DELETE FROM assignments WHERE id = ?", id)
	return err
}

// Held returns the data of every assignment kept, by id.
func (h *History) Held() (map[string][]byte, error) {
	rows, err := h.db.Query("SELECT id, data FROM assignments")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string][]byte{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		found[id] = []byte(data)
	}
	return found, rows.Err()
}

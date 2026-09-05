package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Command run statistics: one command_runs row per invocation of a scheduled
// command, with the counters that command already computes hanging off it in
// command_run_metrics.
//
// The commands this records are fire-and-forget timer jobs whose only output
// today is stdout/stderr in a journal nobody reads. Persisting the run makes
// the admin Statistics page able to answer "did update run, when did it last
// succeed, and how long is suggest taking now" without shell access.
const (
	commandRunStatusRunning = "running"
	commandRunStatusOK      = "ok"
	commandRunStatusPartial = "partial"
	commandRunStatusError   = "error"
)

// commandRunTileBatch is how many per-request rows go in one INSERT. Ten
// columns each keeps this well inside the placeholder limits both engines
// enforce, with room for the cap the producer applies.
const commandRunTileBatch = 100

// commandRunErrorLimit bounds the stored error string. The column is TEXT, but
// a runaway error (a whole API response, say) is noise on a page that shows it
// inline next to a hundred other runs.
const commandRunErrorLimit = 1000

// commandRun accumulates one command's statistics and writes them out. It is
// deliberately infallible from the caller's side: statistics are a diagnostic,
// and a command must never fail — or change its output — because recording it
// did. Every method swallows its own error after reporting it once on stderr,
// and a recorder whose opening INSERT failed simply does nothing thereafter.
type commandRun struct {
	db      *sql.DB
	id      int64
	command string
	started time.Time
	// metrics is keyed by name so a metric cannot be recorded twice for one
	// run; order preserves first-set order for a stable row order on disk.
	metrics map[string]float64
	order   []string
	// tiles is the per-request log a tiled sweep hands over, written out as
	// child rows of the run. It is capped by its producer, not here.
	tiles   []tileAttempt
	partial bool
	live    bool
}

// beginCommandRun inserts the run as 'running' and returns a recorder for it.
//
// The row goes in before the work rather than after it because the case worth
// catching is the run that never comes back: a kill, an OOM, a hang. Recording
// only on completion would leave exactly those invisible. Nothing later flips a
// stale 'running' row — there is no daemon — so readers treat one whose
// started_at is old as interrupted.
//
// The returned recorder is always usable, so callers need no error handling.
func beginCommandRun(ctx context.Context, db *sql.DB, command string) *commandRun {
	run := &commandRun{
		db:      db,
		command: command,
		started: time.Now().UTC(),
		metrics: make(map[string]float64),
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	result, err := db.ExecContext(ctx, `
		INSERT INTO command_runs (command, started_at, status, host, version)
		VALUES (?, ?, ?, ?, ?)
	`, command, run.started.Format(time.RFC3339), commandRunStatusRunning, host, version)
	if err != nil {
		run.fail("record start", err)
		return run
	}
	id, err := result.LastInsertId()
	if err != nil {
		run.fail("record start", err)
		return run
	}
	run.id = id
	run.live = true
	return run
}

// set records one counter under a stable name. Names are the contract the
// admin Statistics page renders, so they are lowercase snake_case and do not
// change once released.
func (r *commandRun) set(name string, value float64) {
	if r == nil || !r.live {
		return
	}
	if _, seen := r.metrics[name]; !seen {
		r.order = append(r.order, name)
	}
	r.metrics[name] = value
}

// setBool records a flag as 0/1 so the page can filter on it in SQL like any
// other metric.
func (r *commandRun) setBool(name string, value bool) {
	if value {
		r.set(name, 1)
		return
	}
	r.set(name, 0)
}

// recordTiles hands over one sweep's individual requests, which are written out
// as child rows of the run. A sweep that made more than its producer keeps
// reports the difference as a counter — see tile_requests_unlogged — so this
// only ever receives the list itself.
//
// Calling it twice replaces the log rather than appending: a run has exactly
// one sweep, and a second call means a caller lost track of that.
func (r *commandRun) recordTiles(attempts []tileAttempt) {
	if r == nil || !r.live {
		return
	}
	r.tiles = attempts
}

// markPartial records that some units of work failed while others succeeded —
// the "2 of 5 cities failed" case. The command still returns its error; this
// only separates a degraded run from a total failure on the page.
func (r *commandRun) markPartial() {
	if r == nil {
		return
	}
	r.partial = true
}

// finish completes the run row and writes the accumulated metrics. Pass the
// command's own return value: the status is derived from it and from whether
// markPartial was called.
func (r *commandRun) finish(ctx context.Context, cmdErr error) {
	if r == nil || !r.live {
		return
	}
	r.live = false

	finished := time.Now().UTC()
	status := commandRunStatusOK
	switch {
	case r.partial:
		status = commandRunStatusPartial
	case cmdErr != nil:
		status = commandRunStatusError
	}
	var errText any
	if cmdErr != nil {
		errText = truncateRunError(cmdErr.Error())
	}

	// A cancelled context is the common way a command ends early, and it would
	// also reject the write that records why. Finish on a fresh context so the
	// run does not stay 'running' for a reason we already know.
	writeCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	tx, err := r.db.BeginTx(writeCtx, nil)
	if err != nil {
		r.fail("record finish", err)
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(writeCtx, `
		UPDATE command_runs
		SET finished_at = ?, duration_ms = ?, status = ?, error = ?
		WHERE id = ?
	`, finished.Format(time.RFC3339), finished.Sub(r.started).Milliseconds(), status, errText, r.id); err != nil {
		r.fail("record finish", err)
		return
	}

	if len(r.order) > 0 {
		var (
			placeholders []string
			args         []any
		)
		for _, name := range r.order {
			placeholders = append(placeholders, "(?, ?, ?)")
			args = append(args, r.id, name, r.metrics[name])
		}
		if _, err := tx.ExecContext(writeCtx,
			`INSERT INTO command_run_metrics (run_id, name, value) VALUES `+strings.Join(placeholders, ", "),
			args...,
		); err != nil {
			r.fail("record metrics", err)
			return
		}
	}

	// The requests go in one statement per batch rather than one per request:
	// a 50 km sweep is a handful of rows, but a sweep of many tiled targets is
	// a few hundred, and that is a round trip each against MySQL.
	for start := 0; start < len(r.tiles); start += commandRunTileBatch {
		end := min(start+commandRunTileBatch, len(r.tiles))
		var (
			placeholders []string
			args         []any
		)
		for i, tile := range r.tiles[start:end] {
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			var errText any
			if tile.Err != "" {
				errText = truncateRunError(tile.Err)
			}
			args = append(args,
				r.id, start+i, tile.City, tile.Tile, tile.Attempt,
				tile.SentAt.UTC().Format(time.RFC3339),
				tile.Waited.Milliseconds(), tile.Duration.Milliseconds(),
				tile.Status, errText,
			)
		}
		if _, err := tx.ExecContext(writeCtx,
			`INSERT INTO command_run_tiles
				(run_id, seq, city, tile_index, attempt, sent_at, waited_ms, duration_ms, status, error)
			VALUES `+strings.Join(placeholders, ", "),
			args...,
		); err != nil {
			r.fail("record tile requests", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		r.fail("record finish", err)
	}
}

// fail reports a statistics write that did not happen and disables the
// recorder. It never returns an error: nothing upstream is allowed to act on
// this.
func (r *commandRun) fail(what string, err error) {
	r.live = false
	fmt.Fprintf(os.Stderr, "command stats: %s for %s: %v\n", what, r.command, err)
}

func truncateRunError(msg string) string {
	if len(msg) <= commandRunErrorLimit {
		return msg
	}
	// Cut on a rune boundary: an error can carry a city or station name, and
	// half a multi-byte rune is not valid utf8mb4.
	cut := commandRunErrorLimit
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

// pruneCommandRuns drops runs older than the retention window and returns how
// many went. Metrics go first so the foreign key holds at every point.
func pruneCommandRuns(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.AddDate(0, 0, -commandRunRetentionDays).UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `
		DELETE FROM command_run_metrics
		WHERE run_id IN (SELECT id FROM command_runs WHERE started_at < ?)
	`, cutoff); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM command_run_tiles
		WHERE run_id IN (SELECT id FROM command_runs WHERE started_at < ?)
	`, cutoff); err != nil {
		return 0, err
	}
	result, err := db.ExecContext(ctx, `DELETE FROM command_runs WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(pruned), nil
}

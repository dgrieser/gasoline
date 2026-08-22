package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ── probes ────────────────────────────────────────────────────────────────────
//
// A probe is a second, read-only measurement taken beside a query doctor is
// timing: the same access path with a narrower projection, or the same filter
// without the joins. The gap between the two prices whatever the narrower one
// leaves out.
//
// This exists because a plan verdict cannot answer the question that matters.
// "covering idx_x" and a bare "idx_x" both say which index was chosen; neither
// says why a query that took the right index still took three minutes. On the
// production database that gap turned out to be one table-row lookup per index
// entry, and the probe is what made that visible — walking 612,665 entries cost
// 262 ms of a 158-second query, so the row count was never the problem and
// reducing it would have been the wrong fix.
//
// Probes are shared by every page doctor measures. They are never queries a page
// issues, so they are reported as sub-lines and never counted into a page's
// total, and --probe=false turns them off.

// doctorProbeSpec is one derived measurement attached to a query spec.
type doctorProbeSpec struct {
	name    string
	purpose string
	sql     string
	args    []any
	alias   string
	// sqlFor rebuilds this probe with one index forced, so it can be pinned to
	// the plan its query actually took. Without it a probe is at the optimizer's
	// mercy, and a narrower projection is enough to change its mind: measured on
	// production, the unpinned probe for `series` chose a different index and
	// came out four times slower than the query it was supposed to be a floor
	// for. Nil where a probe cannot be steered, and then the comparability check
	// below is the only guard.
	sqlFor func(index string) string
}

// measureProbe explains, times and classifies one probe, pinned where it can be
// to the plan its query took.
//
// A probe only means something if it reads the same rows by the same path as its
// query — the gap between the two is then the work the probe leaves out. If the
// plans differ, the gap is the difference between two unrelated plans and says
// nothing, so the probe is marked not comparable and no finding is drawn from
// it.
func measureProbe(ctx context.Context, db *sql.DB, d dialect, spec *doctorProbeSpec,
	opts doctorOptions, indexNames []string, parent doctorQuery) *doctorQueryProbe {
	query, args := spec.sql, spec.args
	if spec.sqlFor != nil && parent.UsesIndex != "" {
		pinned := spec.sqlFor(parent.UsesIndex)
		// A forced index the server will not accept here (SQLite cannot be told
		// to use a rowid, for one) leaves the unpinned probe as the fallback.
		if _, _, err := explainPlan(ctx, db, d, pinned, args, false); err == nil {
			query = pinned
		}
	}

	out := &doctorQueryProbe{Name: spec.name, Purpose: spec.purpose, SQL: query}
	plan, cells, err := explainPlan(ctx, db, d, query, args, opts.Analyze)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Plan = plan
	out.UsesIndex, out.CoveringHit, out.FullScan = classifyPlan(plan, cells, d, indexNames, spec.alias)

	started := time.Now()
	count, err := countQueryRows(ctx, db, query, args)
	out.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		out.Error = err.Error()
	}
	out.Rows = count
	out.Comparable = out.Error == "" && out.UsesIndex == parent.UsesIndex
	return out
}

// A share of a query's time is not on its own a reason to warn: 4 ms out of
// 7 ms is half the query and still nobody's problem. Both an absolute floor and
// a share have to be crossed, and below the note floor there is nothing worth
// saying at all.
const (
	doctorLookupNoteMS = 5
	doctorLookupWarnMS = 100
)

// lookupSeverity rates the time a query spends on work its index could have
// carried instead. The second return says whether it is worth reporting.
func lookupSeverity(lookupsMS, totalMS, slowMS float64) (string, bool) {
	if lookupsMS < doctorLookupNoteMS {
		return "", false
	}
	if lookupsMS >= slowMS || (lookupsMS >= doctorLookupWarnMS && lookupsMS >= totalMS/2) {
		return "warn", true
	}
	return "info", true
}

// perLookupMicros is what one row lookup cost. It is the number that separates a
// query reading a lot of rows from a cache (a few microseconds each) from one
// seeking to disk for every one of them, and those need different fixes.
func perLookupMicros(lookupsMS float64, rows int) float64 {
	if rows <= 0 {
		return 0
	}
	return lookupsMS * 1000 / float64(rows)
}

// doctorSeekMicros is where a per-lookup cost stops looking like a cache hit.
// Above it the table is being read from disk a row at a time, and then reducing
// the row count is treating a symptom.
const doctorSeekMicros = 50

// lookupRate renders the per-lookup cost, and says so when it is seek latency
// rather than a cache hit — the two want different fixes, so the distinction is
// worth a sentence wherever a finding reports lookups at all.
func lookupRate(lookupsMS float64, rows int) string {
	perLookup := perLookupMicros(lookupsMS, rows)
	out := fmt.Sprintf("about %.0f µs each", perLookup)
	if perLookup >= doctorSeekMicros {
		out += ". That is seek latency rather than a cache hit, so the table is not staying in the buffer pool"
	}
	return out
}

// indexOrPlan names the index a query took, or says there was none, so a
// finding reads the same either way.
func indexOrPlan(q doctorQuery) string {
	if q.UsesIndex == "" {
		return "the plan it chose"
	}
	return q.UsesIndex
}

// probeLookupFinding is the finding shape every lookup-bound query produces: how
// much of its time went on work the index could have carried, at what rate per
// row, with `detail` naming what that work actually was.
func probeLookupFinding(q doctorQuery, detail string, opts doctorOptions) (doctorFinding, bool) {
	probe := q.Probe
	if probe == nil || probe.Error != "" || q.Error != "" || !probe.Comparable {
		return doctorFinding{}, false
	}
	lookups := q.DurationMS - probe.DurationMS
	severity, report := lookupSeverity(lookups, q.DurationMS, opts.SlowMS)
	if !report {
		return doctorFinding{}, false
	}
	rows := q.Rows
	if probe.Rows > rows {
		// An aggregate returns fewer rows than it reads; the rate that means
		// anything is per row read, not per row returned.
		rows = probe.Rows
	}
	return doctorFinding{
		Severity: severity,
		Message: fmt.Sprintf("query %s spends %.0f ms of its %.0f ms %s, %s",
			q.Name, lookups, q.DurationMS, detail, lookupRate(lookups, rows)),
	}, true
}

// writeDoctorProbeText prints a probe as a sub-line of the query it belongs to,
// indented so it never reads as a query of its own.
func writeDoctorProbeText(p *doctorQueryProbe, opts doctorOptions, explain bool) {
	if p == nil {
		return
	}
	note := "no index"
	switch {
	case p.Error != "":
		note = "failed: " + p.Error
	case p.FullScan:
		note = "TABLE SCAN"
	case p.UsesIndex != "" && p.CoveringHit:
		note = "covering " + p.UsesIndex
	case p.UsesIndex != "":
		note = p.UsesIndex
	}
	// A probe on a different plan is not a floor for anything, and saying so on
	// the line is what keeps its timing from being read as one.
	if p.Error == "" && !p.Comparable {
		note += "  (different plan, not comparable)"
	}
	fmt.Fprintf(stdout, "    %-16s %9.1f ms %8d rows  %s\n",
		"probe/"+p.Name, p.DurationMS, p.Rows, note)
	if opts.ShowSQL {
		fmt.Fprintf(stdout, "      probe sql: %s\n", p.SQL)
	}
	if explain {
		for _, line := range p.Plan {
			fmt.Fprintf(stdout, "      probe | %s\n", line)
		}
	}
}

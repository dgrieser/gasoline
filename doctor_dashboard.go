package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── dashboard page diagnostics ────────────────────────────────────────────────
//
// The admin accuracy page got this treatment first (accuracyQuerySpecsFor): its
// SQL is mirrored here so doctor can run it against a live database, time it and
// say which index the planner actually took. The dashboard is the other page
// whose load time an operator complains about, and it is slow for entirely
// different reasons, so it needs its own mirror rather than a shared one.
//
// What a dashboard load does, in the order web/index.php does it: read the
// reader's stored filters, resolve the stations in scope for that location and
// radius (loadScopeStations), read the current price at the nearest of them
// (loadNearbyPrices), read the snapshot history for the selected ones over the
// date filter (buildSnapshotQuery), then read the upcoming fill-up windows
// (loadFilteredPredictions). doctor reproduces all of it, including the station
// list the page would have inlined into IN(...) — the length of that list is
// part of the cost, so guessing it would defeat the point.
//
// The filter read itself is not among them: it is one primary-key row out of
// user_filters, and timing that would tell an operator nothing. It is why the
// page no longer resolves a city on load, though, which is why --city is now
// doctor's own stand-in for a reader's stored location rather than something
// the page looks up.
//
// dashboardQuerySpecsFor documents that duplication the same way the accuracy
// side does, and TestDoctorDashboardQueriesMatchViewer fails if the two drift.

// dashboardRadiusOptions are the radii the page's dropdown offers. doctor
// accepts exactly these, because a value the page cannot produce would not be
// reproducing a page load.
var dashboardRadiusOptions = []int{5, 10, 20}

// dashboardNearbyLimit mirrors NEARBY_STATION_LIMIT in web/index.php: how many
// of the nearest stations the surroundings card reads a current price for, and
// so how many correlated seeks that query costs at most.
const dashboardNearbyLimit = 40

// dashboardFuels expands the page's fuel filter: "all" loads three fuels, which
// triples what the prediction queries read.
func dashboardFuels(fuel string) []string {
	if fuel == "all" {
		return []string{"e5", "e10", "diesel"}
	}
	return []string{fuel}
}

// doctorDashboardFilters is the filter state the page would have loaded. Every
// field maps to one column of the reader's user_filters row.
type doctorDashboardFilters struct {
	// City stands in for the reader's stored location: doctor takes a city
	// name because that is what an operator can type, and resolves it to the
	// coordinates the filter row would have held. Empty for the unscoped view.
	City string `json:"city"`
	// CityAuto marks a city doctor picked rather than one the operator named.
	CityAuto bool     `json:"city_auto"`
	RadiusKM int      `json:"radius_km"`
	Fuel     string   `json:"fuel"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Stations []string `json:"stations,omitempty"`
}

// doctorDashboardScope is what the page's own scope resolution produced, which
// is the input to every query after it. The three counts narrow in sequence, and
// which step does the narrowing is worth seeing: a bounding box that admits ten
// times the stations the radius keeps means ten times the freshness probes.
type doctorDashboardScope struct {
	// Candidates is what the bounding-box-plus-freshness query returned.
	Candidates int `json:"bbox_candidates"`
	// Stations is what survived the haversine cut to the exact radius.
	Stations int `json:"stations"`
	// Selected is what the station picker left, and what lands in IN(...).
	Selected  int  `json:"selected"`
	CityFound bool `json:"city_found"`
}

// doctorDashboard is the dashboard half of a doctor report.
type doctorDashboard struct {
	Filters doctorDashboardFilters `json:"filters"`
	Scope   doctorDashboardScope   `json:"scope"`
	Queries []doctorQuery          `json:"queries"`
	// Fuels is the expansion of Filters.Fuel, because "all" is three fuels
	// worth of prediction rows and that is not obvious from the filter alone.
	Fuels []string `json:"fuels"`
	// Skipped marks a run that measured nothing, so the filters and scope
	// counts below are unresolved rather than zero.
	Skipped bool `json:"skipped,omitempty"`
}

// dashboardQuerySpec is one query a dashboard load issues, optionally with a
// probe that prices part of its cost.
type dashboardQuerySpec struct {
	name    string
	purpose string
	sql     string
	args    []any
	// table and alias name the large table the query drives from, so plan
	// classification can be scoped to it (see classifyPlan).
	table string
	alias string
	probe *doctorProbeSpec
}

// dashboardQueryContext is everything that shapes a dashboard load's SQL.
type dashboardQueryContext struct {
	Filters doctorDashboardFilters
	// StationIDs is the resolved in-scope selection, exactly as the page would
	// inline it into IN(...).
	StationIDs []string
	// FreshCutoff is the station-freshness bound loadScopeStations applies.
	FreshCutoff string
	// BBox is the bounding box for the selected city and radius; nil for the
	// unscoped view.
	BBox *dashboardBBox
	// ScopeStationIDs is every station the radius admits, before the picker
	// narrows it. The surroundings card reads a current price for the nearest
	// of them and is deliberately not narrowed by the picker, so it is that
	// list — not StationIDs — that sizes its query.
	ScopeStationIDs []string
	// CityMissing marks a named city the cities table does not hold. doctor
	// cannot place a bounding box without one, so there is no scope query to
	// measure past it — the operator named somewhere doctor cannot stand.
	CityMissing bool
	// Now is the instant the prediction grid is cut at.
	Now time.Time
}

// dashboardBBox mirrors the viewer's boundingBox(): a cheap latitude/longitude
// pre-filter the exact haversine distance then narrows.
type dashboardBBox struct {
	MinLat, MaxLat, MinLng, MaxLng float64
}

// dashboardBoundingBox mirrors boundingBox() in web/index.php, constants
// included — the page's station scope is what doctor has to reproduce, so the
// approximation has to be the same one.
func dashboardBoundingBox(lat, lng float64, radiusKM int) dashboardBBox {
	latDelta := float64(radiusKM) / 111.32
	lngDivisor := 111.32 * math.Max(math.Cos(lat*math.Pi/180), 0.01)
	lngDelta := float64(radiusKM) / lngDivisor
	return dashboardBBox{
		MinLat: lat - latDelta,
		MaxLat: lat + latDelta,
		MinLng: lng - lngDelta,
		MaxLng: lng + lngDelta,
	}
}

// boundPlaceholders renders n bound parameters, which is how the page builds its
// IN(...) lists. The station ids are user data and stay bound.
func boundPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// stationScopeSQL is the freshness test loadScopeStations applies to every
// candidate station: one seek into idx_price_snapshots_station_recorded rather
// than an aggregate over the snapshot history.
const stationScopeSQL = "EXISTS (SELECT 1 FROM price_snapshots fresh " +
	"WHERE fresh.station_id = s.id AND fresh.recorded_at >= ?)"

// stationScopeColumns is the projection loadScopeStations reads. It is spelled
// out rather than reduced to what doctor needs, because the column list is part
// of what the query costs.
const stationScopeColumns = "s.id, COALESCE(s.name_override, s.name) AS name, " +
	"COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand, " +
	"TRIM(COALESCE(s.street, '')) AS street, " +
	"TRIM(COALESCE(s.house_number, '')) AS house_number, " +
	"s.post_code, TRIM(COALESCE(s.place, '')) AS place, " +
	"s.last_seen_at, s.lat, s.lng"

// dashboardQuerySpecsFor builds the queries one dashboard load issues.
//
// The last ones are skipped by the page itself and so are absent here when
// there is no scope: with neither a location nor a station selection,
// buildSnapshotQuery and loadFilteredPredictions are never reached (see the "no
// scope to display" guard in web/index.php), and without a location there is no
// "around here" for loadNearbyPrices to answer. A city doctor cannot resolve
// stops it earlier still, before it can place a bounding box.
func dashboardQuerySpecsFor(qc dashboardQueryContext) []dashboardQuerySpec {
	f := qc.Filters
	var specs []dashboardQuerySpec

	if f.City != "" {
		// The location field is a typeahead, so a dashboard visit that changes
		// where the reader is runs this. Three letters is the page's own
		// minimum. Its postal-code half is not mirrored: it only fires for an
		// all-digit term, and it reads the stations table rather than the
		// history, so it is not what makes a dashboard slow.
		if term := citySearchTerm(f.City); term != "" {
			specs = append(specs, dashboardQuerySpec{
				name:    "city_search",
				purpose: "location typeahead, first 3 letters (?action=city_search)",
				sql: "SELECT normalized_name AS city_key, display_name, lat, lng FROM cities " +
					"WHERE normalized_lower >= ? AND normalized_lower < ? " +
					"ORDER BY normalized_lower ASC LIMIT 20",
				// The page's own upper bound: the prefix with the highest code
				// point appended, which no character following the prefix can
				// reach. See the city_search handler in web/index.php.
				args:  []any{term, term + string(rune(0x10FFFF))},
				table: "cities",
				alias: "cities",
			})
		}
	}
	if qc.CityMissing {
		return specs
	}

	if qc.BBox != nil {
		specs = append(specs, dashboardQuerySpec{
			name:    "scope_stations",
			purpose: "stations inside the radius and still being fed (loadScopeStations)",
			sql: "SELECT " + stationScopeColumns + " FROM stations s " +
				"WHERE s.lat BETWEEN ? AND ? AND s.lng BETWEEN ? AND ? AND " + stationScopeSQL,
			args: []any{qc.BBox.MinLat, qc.BBox.MaxLat, qc.BBox.MinLng, qc.BBox.MaxLng,
				qc.FreshCutoff},
			table: "stations",
			alias: "s",
		})
	} else {
		specs = append(specs, dashboardQuerySpec{
			name:    "scope_stations",
			purpose: "every station still being fed (loadScopeStations, unscoped)",
			sql: "SELECT " + stationScopeColumns + " FROM stations s WHERE " + stationScopeSQL +
				" ORDER BY COALESCE(s.name_override, s.name) ASC, s.id ASC",
			args:  []any{qc.FreshCutoff},
			table: "stations",
			alias: "s",
		})
	}

	// ── surroundings ──────────────────────────────────────────────────────
	// loadNearbyPrices: the current price at each of the nearest stations. Its
	// scope is the radius alone, so it runs whenever a location is selected —
	// including when the picker has narrowed everything below it away.
	//
	// Both halves are bounded by the freshness window, which is the point of
	// its shape: the station list is already known to have been fed inside that
	// window, so neither half ever has to walk a station's full history.
	if qc.BBox != nil && len(qc.ScopeStationIDs) > 0 {
		nearbyIDs := qc.ScopeStationIDs
		if len(nearbyIDs) > dashboardNearbyLimit {
			nearbyIDs = nearbyIDs[:dashboardNearbyLimit]
		}
		nearbyIn := boundPlaceholders(len(nearbyIDs))
		// The grouped half's list and cutoff, then the outer half's, in the
		// order the page binds them.
		nearbyInner := make([]any, 0, len(nearbyIDs)+1)
		for _, id := range nearbyIDs {
			nearbyInner = append(nearbyInner, id)
		}
		nearbyInner = append(nearbyInner, qc.FreshCutoff)
		nearbyArgs := append(append([]any{}, nearbyInner...), nearbyInner...)
		nearbyNewest := "SELECT station_id, MAX(recorded_at) AS newest_at FROM price_snapshots " +
			"WHERE station_id IN (" + nearbyIn + ") AND recorded_at >= ? GROUP BY station_id"
		specs = append(specs, dashboardQuerySpec{
			name:    "nearby_latest",
			purpose: "the current price at each of the nearest stations (loadNearbyPrices)",
			sql: "SELECT ps.station_id, ps.recorded_at, ps.is_open, " +
				raisedNinePriceSQL("ps.e5") + " AS e5, " +
				raisedNinePriceSQL("ps.e10") + " AS e10, " +
				raisedNinePriceSQL("ps.diesel") + " AS diesel " +
				"FROM price_snapshots ps JOIN (" + nearbyNewest + ") newest " +
				"ON newest.station_id = ps.station_id AND newest.newest_at = ps.recorded_at " +
				"WHERE ps.station_id IN (" + nearbyIn + ") AND ps.recorded_at >= ?",
			args:  nearbyArgs,
			table: "price_snapshots",
			alias: "ps",
			probe: &doctorProbeSpec{
				name:    "newest only",
				purpose: "the grouped lookup alone, so the difference is the join back and the row lookups",
				sql:     nearbyNewest,
				args:    nearbyInner,
				alias:   "price_snapshots",
			},
		})
	}

	if len(qc.StationIDs) == 0 {
		return specs
	}

	stationIn := boundPlaceholders(len(qc.StationIDs))
	stationArgs := make([]any, 0, len(qc.StationIDs))
	for _, id := range qc.StationIDs {
		stationArgs = append(stationArgs, id)
	}

	// ── snapshots ─────────────────────────────────────────────────────────
	// buildSnapshotQuery appends its predicates in this order — lower bound,
	// upper bound, station list — and the bound values follow it.
	var snapWhere []string
	var snapArgs []any
	if f.From != "" {
		snapWhere = append(snapWhere, "ps.recorded_at >= ?")
		snapArgs = append(snapArgs, f.From)
	}
	if f.To != "" {
		snapWhere = append(snapWhere, "ps.recorded_at <= ?")
		snapArgs = append(snapArgs, f.To)
	}
	snapWhere = append(snapWhere, "ps.station_id IN ("+stationIn+")")
	snapArgs = append(snapArgs, stationArgs...)
	snapFilter := strings.Join(snapWhere, " AND ")
	snapOrder := " ORDER BY ps.recorded_at ASC, ps.station_id ASC"
	specs = append(specs, dashboardQuerySpec{
		name:    "snapshots",
		purpose: "the price history the chart and table are drawn from (buildSnapshotQuery)",
		sql: "SELECT ps.station_id, ps.recorded_at, ps.is_open, " +
			raisedNinePriceSQL("ps.e5") + " AS e5, " +
			raisedNinePriceSQL("ps.e10") + " AS e10, " +
			raisedNinePriceSQL("ps.diesel") + " AS diesel " +
			"FROM price_snapshots ps WHERE " + snapFilter + snapOrder,
		args:  snapArgs,
		table: "price_snapshots",
		alias: "ps",
		probe: &doctorProbeSpec{
			name:    "keys only",
			purpose: "the same rows without the price columns, so the difference is the row lookups",
			sql: "SELECT ps.station_id, ps.recorded_at FROM price_snapshots ps " +
				"WHERE " + snapFilter + snapOrder,
			args:  snapArgs,
			alias: "ps",
		},
	})

	// ── predictions ───────────────────────────────────────────────────────
	fuels := dashboardFuels(f.Fuel)
	fuelIn := boundPlaceholders(len(fuels))
	predArgs := append([]any{}, stationArgs...)
	for _, fuel := range fuels {
		predArgs = append(predArgs, fuel)
	}
	predWhere := "pp.station_id IN (" + stationIn + ") AND pp.fuel IN (" + fuelIn + ")"

	// The page binds RFC3339 with a numeric offset here, which is what it
	// compares against target_start values stored with a trailing Z. Mirroring
	// the format keeps doctor's row counts equal to the page's.
	//
	// This is the only prediction query left. There used to be a second one
	// resolving the newest run per station and fuel, which bounded station and
	// fuel but nothing in time and so aggregated over the whole retention
	// window; doctor measured it at 158 s against 612,665 rows on production and
	// the page now derives the same answer from these rows instead.
	nowBound := qc.Now.UTC().Format("2006-01-02T15:04:05-07:00")
	gridArgs := append(append([]any{}, predArgs...), nowBound)
	gridWhere := predWhere + " AND pp.target_start > ?"
	gridOrder := " ORDER BY pp.target_start ASC, pp.station_id ASC"
	specs = append(specs, dashboardQuerySpec{
		name:    "predictions_grid",
		purpose: "future forecast windows for the scope, reduced to the newest run per station in PHP",
		sql: "SELECT pp.station_id, pp.fuel, pp.run_id, pp.target_start, pp.target_end, " +
			"pp.predicted_price, pp.confidence, pr.run_at, pr.suggestion_bias " +
			"FROM price_predictions pp JOIN prediction_runs pr ON pr.id = pp.run_id " +
			"WHERE " + gridWhere + gridOrder,
		args:  gridArgs,
		table: "price_predictions",
		alias: "pp",
		probe: &doctorProbeSpec{
			name:    "keys only",
			purpose: "the same rows without the payload columns or the run join",
			sql: "SELECT pp.station_id, pp.fuel, pp.target_start FROM price_predictions pp " +
				"WHERE " + gridWhere + gridOrder,
			args:  gridArgs,
			alias: "pp",
		},
	})

	return specs
}

// raisedNinePriceSQL mirrors raisedNinePriceSql() in web/index.php: the board
// price normalization the snapshot projection applies to every row.
func raisedNinePriceSQL(column string) string {
	milli := "ROUND(" + column + " * 1000)"
	return "CASE WHEN " + column + " > 0 THEN (" + milli + " - " + milli + " % 10 + 9) / 1000.0 ELSE " + column + " END"
}

// citySearchTerm is the typeahead prefix the city dropdown would have sent for
// a city: its first three letters, lowercased, which is the page's minimum.
func citySearchTerm(city string) string {
	letters := []rune(strings.ToLower(city))
	if len(letters) < 3 {
		return ""
	}
	return string(letters[:3])
}

// resolveDashboardRange mirrors the page's date defaults: an explicit from
// and/or to, or the last 7 days. Unlike the accuracy page the upper bound is
// normally open — the page clears "to" whenever a named range is active — and
// that openness is deliberate, so doctor keeps it.
func resolveDashboardRange(rangeName, from, to string, now time.Time) (string, string, error) {
	now = now.UTC()
	rangeName = strings.TrimSpace(rangeName)
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)

	if rangeName != "" && (from != "" || to != "") {
		return "", "", errors.New("--range cannot be combined with --from/--to")
	}
	if from != "" || to != "" {
		if from != "" && !datePattern.MatchString(from) {
			return "", "", errors.New("--from must be a YYYY-MM-DD date")
		}
		if to != "" && !datePattern.MatchString(to) {
			return "", "", errors.New("--to must be a YYYY-MM-DD date")
		}
		lower, upper := "", ""
		if from != "" {
			lower = from + "T00:00:00Z"
		}
		if to != "" {
			upper = to + "T23:59:59Z"
		}
		return lower, upper, nil
	}

	days := 7
	switch rangeName {
	case "", "7d":
	case "14d":
		days = 14
	case "30d":
		days = 30
	default:
		return "", "", errors.New("--range must be one of: 7d, 14d, 30d")
	}
	return now.AddDate(0, 0, -days).Format("2006-01-02") + "T00:00:00Z", "", nil
}

// resolveDashboardRadius mirrors the page's dropdown, which is the only place a
// radius can come from.
func resolveDashboardRadius(value int) (int, error) {
	for _, option := range dashboardRadiusOptions {
		if value == option {
			return value, nil
		}
	}
	names := make([]string, 0, len(dashboardRadiusOptions))
	for _, option := range dashboardRadiusOptions {
		names = append(names, strconv.Itoa(option))
	}
	return 0, fmt.Errorf("--radius must be one of the radii the dashboard offers: %s", strings.Join(names, ", "))
}

// parseStationList splits a comma-separated --station value, preserving order
// and dropping duplicates so the IN(...) list matches what the picker sends.
func parseStationList(list string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range strings.Split(list, ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// dashboardAutoCity picks the city a plain `doctor dashboard` should measure:
// the one owning the most in-scope stations, which is the busiest dashboard a
// visitor could load. Without it the command would default to the unscoped
// view, where the page skips the two expensive queries entirely and doctor
// would report a fast dashboard while the operator watches a slow one.
func dashboardAutoCity(scope doctorScope) string {
	best := ""
	bestInScope := 0
	for _, city := range scope.Cities {
		if city.InScope > bestInScope || (city.InScope == bestInScope && city.City < best) {
			best, bestInScope = city.City, city.InScope
		}
	}
	if bestInScope == 0 {
		return ""
	}
	return best
}

// dashboardScopeStation is one row of the page's station scope: the coordinates
// are what the haversine cut needs, the id is what lands in IN(...).
type dashboardScopeStation struct {
	ID  string
	Lat float64
	Lng float64
}

// resolveDashboardStations reproduces loadScopeStations: the bounding-box and
// freshness query, then the exact-radius haversine cut, then the station
// picker's intersection. The result is the IN(...) list the page would build,
// which every query after it depends on.
//
// The bounding-box query is measured separately as scope_stations; this pass is
// the same read done for its result rather than its timing, so the two are
// deliberately not shared: a spec measures, this resolves.
func resolveDashboardStations(ctx context.Context, db *sql.DB, qc dashboardQueryContext, cityLat, cityLng float64) (candidates int, inScope []string, err error) {
	where := "WHERE " + stationScopeSQL
	args := []any{qc.FreshCutoff}
	if qc.BBox != nil {
		where = "WHERE s.lat BETWEEN ? AND ? AND s.lng BETWEEN ? AND ? AND " + stationScopeSQL
		args = []any{qc.BBox.MinLat, qc.BBox.MaxLat, qc.BBox.MinLng, qc.BBox.MaxLng, qc.FreshCutoff}
	}
	rows, err := db.QueryContext(ctx, "SELECT s.id, s.lat, s.lng FROM stations s "+where, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var found []dashboardScopeStation
	for rows.Next() {
		var station dashboardScopeStation
		if err := rows.Scan(&station.ID, &station.Lat, &station.Lng); err != nil {
			return 0, nil, err
		}
		found = append(found, station)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	type scoped struct {
		id   string
		dist float64
	}
	var kept []scoped
	for _, station := range found {
		if qc.BBox == nil {
			kept = append(kept, scoped{id: station.ID})
			continue
		}
		// haversineKM differs from the viewer's own haversine only in the earth
		// radius it uses (6371.0088 km against 6371 km), which moves a distance
		// by 1.4 parts per million — millimetres at these radii.
		dist := haversineKM(cityLat, cityLng, station.Lat, station.Lng)
		if dist > float64(qc.Filters.RadiusKM) {
			continue
		}
		kept = append(kept, scoped{id: station.ID, dist: dist})
	}
	if qc.BBox != nil {
		sort.SliceStable(kept, func(i, j int) bool {
			if kept[i].dist != kept[j].dist {
				return kept[i].dist < kept[j].dist
			}
			return kept[i].id < kept[j].id
		})
	}
	ids := make([]string, 0, len(kept))
	for _, station := range kept {
		ids = append(ids, station.id)
	}
	return len(found), ids, nil
}

// dashboardCityCentre reads the coordinates the page resolved the city to.
func dashboardCityCentre(ctx context.Context, db *sql.DB, city string) (lat, lng float64, found bool, err error) {
	err = db.QueryRowContext(ctx,
		"SELECT lat, lng FROM cities WHERE normalized_name = ? LIMIT 1", city).Scan(&lat, &lng)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return lat, lng, true, nil
}

// runDashboardChecks measures one dashboard load. Read-only throughout.
func runDashboardChecks(ctx context.Context, db *sql.DB, d dialect, opts doctorOptions,
	present map[string]bool, indexesByTable map[string][]string, scope doctorScope, now time.Time) *doctorDashboard {
	filters := opts.Dashboard
	dash := &doctorDashboard{Filters: filters, Fuels: dashboardFuels(filters.Fuel)}

	for _, needed := range []string{"stations", "price_snapshots", "cities"} {
		if !present[needed] {
			dash.Skipped = true
			dash.Queries = append(dash.Queries, doctorQuery{
				Name:    "dashboard",
				Purpose: "skipped: " + needed + " does not exist — run `gasoline migrate`",
				Skipped: true,
			})
			return dash
		}
	}
	if opts.SkipQueries {
		dash.Skipped = true
		dash.Queries = append(dash.Queries, doctorQuery{
			Name:    "dashboard",
			Purpose: "skipped via --skip-queries",
			Skipped: true,
		})
		return dash
	}

	// An unnamed city means doctor picks the busiest one, so a plain
	// `doctor dashboard` measures a dashboard somebody could actually load.
	if filters.City == "" && !opts.DashboardNoCity {
		if picked := dashboardAutoCity(scope); picked != "" {
			filters.City = picked
			filters.CityAuto = true
		}
	}

	qc := dashboardQueryContext{
		Filters:     filters,
		FreshCutoff: now.Add(-stationFreshness).UTC().Format(time.RFC3339),
		Now:         now,
	}

	if filters.City != "" {
		lat, lng, found, err := dashboardCityCentre(ctx, db, filters.City)
		if err != nil {
			dash.Queries = append(dash.Queries, doctorQuery{
				Name: "city", Purpose: "resolve the selected city", Error: err.Error(),
			})
			dash.Filters = filters
			return dash
		}
		dash.Scope.CityFound = found
		if found {
			box := dashboardBoundingBox(lat, lng, filters.RadiusKM)
			qc.BBox = &box
		}
		// A city the page cannot resolve makes it stop before any scope query,
		// and doctor stops with it — but the city lookup itself is still worth
		// timing, since that is the query that failed to find anything.
		if !found {
			dash.Filters = filters
			qc.CityMissing = true
			dash.Queries = runDashboardSpecs(ctx, db, d, opts, indexesByTable, dashboardQuerySpecsFor(qc))
			return dash
		}
		centreLat, centreLng := lat, lng
		candidates, ids, err := resolveDashboardStations(ctx, db, qc, centreLat, centreLng)
		if err != nil {
			dash.Filters = filters
			dash.Queries = append(dash.Queries, doctorQuery{
				Name: "scope_stations", Purpose: "resolve the stations in scope", Error: err.Error(),
			})
			return dash
		}
		dash.Scope.Candidates = candidates
		dash.Scope.Stations = len(ids)
		qc.ScopeStationIDs = ids
		qc.StationIDs = intersectStationSelection(ids, filters.Stations)
	} else {
		// The unscoped view: the page loads the station list for the sidebar
		// but skips the snapshot and prediction queries unless the visitor has
		// picked stations by hand.
		candidates, ids, err := resolveDashboardStations(ctx, db, qc, 0, 0)
		if err != nil {
			dash.Filters = filters
			dash.Queries = append(dash.Queries, doctorQuery{
				Name: "scope_stations", Purpose: "resolve the stations in scope", Error: err.Error(),
			})
			return dash
		}
		dash.Scope.Candidates = candidates
		dash.Scope.Stations = len(ids)
		qc.StationIDs = filters.Stations
	}
	dash.Scope.Selected = len(qc.StationIDs)
	dash.Filters = filters

	dash.Queries = runDashboardSpecs(ctx, db, d, opts, indexesByTable, dashboardQuerySpecsFor(qc))
	return dash
}

// intersectStationSelection mirrors the page's array_intersect: the picker can
// only narrow the in-scope list, never add to it.
func intersectStationSelection(inScope, selected []string) []string {
	if len(selected) == 0 {
		return inScope
	}
	wanted := map[string]bool{}
	for _, id := range selected {
		wanted[id] = true
	}
	out := make([]string, 0, len(inScope))
	for _, id := range inScope {
		if wanted[id] {
			out = append(out, id)
		}
	}
	return out
}

// runDashboardSpecs explains, times and classifies each query, plus its probe.
func runDashboardSpecs(ctx context.Context, db *sql.DB, d dialect, opts doctorOptions,
	indexesByTable map[string][]string, specs []dashboardQuerySpec) []doctorQuery {
	out := make([]doctorQuery, 0, len(specs))
	for _, spec := range specs {
		q := doctorQuery{Name: spec.name, Purpose: spec.purpose, SQL: spec.sql, Table: spec.table}
		plan, cells, err := explainPlan(ctx, db, d, spec.sql, spec.args, opts.Analyze)
		if err != nil {
			q.Error = err.Error()
		} else {
			q.Plan = plan
			q.UsesIndex, q.CoveringHit, q.FullScan = classifyPlan(plan, cells, d, indexesByTable[spec.table], spec.alias)
			q.Considered = consideredIndexes(cells, spec.alias, q.UsesIndex)
		}

		timed := timeQuery(ctx, db, spec.sql, spec.args, opts.Runs)
		q.DurationMS, q.SpreadMS, q.Rows = timed.DurationMS, timed.SpreadMS, timed.Rows
		if timed.Err != nil {
			q.Error = timed.Err.Error()
		}

		if spec.probe != nil && opts.Probe {
			q.Probe = measureProbe(ctx, db, d, spec.probe, opts, indexesByTable[spec.table], q)
		}
		out = append(out, q)
	}
	return out
}

// doctorDashboardFindings turns the dashboard measurements into the short list
// an operator acts on. The generic verdicts (a failed query, a slow one, a table
// scan) mirror the accuracy page's; the rest are specific to how this page is
// built, because its two expensive queries are expensive for structural reasons
// that no index choice can fix on its own.
func doctorDashboardFindings(dash *doctorDashboard, tables []doctorTable, opts doctorOptions) []doctorFinding {
	if dash == nil {
		return nil
	}
	var findings []doctorFinding
	warn := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "warn", Message: fmt.Sprintf(format, a...)})
	}
	info := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "info", Message: fmt.Sprintf(format, a...)})
	}

	rowsByTable := map[string]int64{}
	for _, t := range tables {
		rowsByTable[t.Name] = t.Rows
	}
	const scanRowsThreshold = 100_000

	if dash.Filters.City != "" && !dash.Scope.CityFound {
		warn("dashboard city %q is not in the cities table, so the page resolves no scope and shows nothing — check the spelling against `gasoline list cities`",
			dash.Filters.City)
	}
	if dash.Filters.CityAuto && dash.Filters.City != "" {
		info("no --city given, so the dashboard check measured %s, the city with the most stations in scope; pass --city to reproduce another one, or --no-city for the unscoped view",
			dash.Filters.City)
	}
	if !dash.Skipped && dash.Filters.City == "" && dash.Scope.Selected == 0 {
		info("with neither a city nor --station the page has no scope, so it skips the snapshot and prediction queries; this run only measured the station list")
	}
	if dash.Scope.Candidates > 0 && dash.Scope.Stations > 0 && dash.Scope.Candidates >= dash.Scope.Stations*3 {
		info("the %d km bounding box admits %d stations for the %d that survive the exact radius, so %d freshness probes are spent on stations the page then discards",
			dash.Filters.RadiusKM, dash.Scope.Candidates, dash.Scope.Stations,
			dash.Scope.Candidates-dash.Scope.Stations)
	}

	var total float64
	var slowest doctorQuery
	byName := map[string]doctorQuery{}
	for _, q := range dash.Queries {
		if q.Skipped {
			info("dashboard queries %s", q.Purpose)
			continue
		}
		byName[q.Name] = q
		total += q.DurationMS
		if q.DurationMS > slowest.DurationMS {
			slowest = q
		}
		if q.Error != "" {
			warn("dashboard query %s failed: %s", q.Name, q.Error)
			continue
		}
		if q.DurationMS >= opts.SlowMS {
			warn("dashboard query %s took %.0f ms (%s)", q.Name, q.DurationMS, q.Purpose)
		}
		switch {
		case q.FullScan && (rowsByTable[q.Table] >= scanRowsThreshold || q.DurationMS >= opts.SlowMS):
			warn("dashboard query %s scans %s (%s rows) instead of using an index",
				q.Name, q.Table, formatCount(rowsByTable[q.Table]))
		case q.FullScan:
			info("dashboard query %s scans %s, which is small enough for that to be reasonable", q.Name, q.Table)
		case q.UsesIndex == "":
			info("dashboard query %s uses no %s index (plan: %s)", q.Name, q.Table, strings.Join(q.Plan, " | "))
		}
	}

	findings = append(findings, dashboardSnapshotFindings(byName, opts)...)
	findings = append(findings, dashboardPredictionFindings(byName, opts)...)

	if total > 0 {
		info("dashboard page SQL total %.0f ms; slowest %s at %.0f ms", total, slowest.Name, slowest.DurationMS)
	}
	if !opts.Probe {
		info("probes were skipped (--probe=false), so the report cannot say how much of each query's time is row lookups")
	}
	return findings
}

// dashboardSnapshotFindings prices the snapshot history read, which is the query
// whose cost scales with the date filter. Its index stops at
// (station_id, recorded_at); every price column comes from a table row, and the
// probe says what those lookups cost.
func dashboardSnapshotFindings(byName map[string]doctorQuery, opts doctorOptions) []doctorFinding {
	q, ok := byName["snapshots"]
	if !ok || q.Error != "" {
		return nil
	}
	var findings []doctorFinding
	if q.Rows > 0 {
		findings = append(findings, doctorFinding{
			Severity: "info",
			Message: fmt.Sprintf("the dashboard ships %s snapshot rows to the browser for this scope; a wider date filter or radius scales this linearly",
				formatCount(int64(q.Rows))),
		})
	}
	probe := q.Probe
	if probe == nil || probe.Error != "" || q.CoveringHit {
		return findings
	}
	lookups := q.DurationMS - probe.DurationMS
	severity, report := lookupSeverity(lookups, q.DurationMS, opts.SlowMS)
	if !report {
		return findings
	}
	findings = append(findings, doctorFinding{
		Severity: severity,
		Message: fmt.Sprintf("snapshots spends %.0f ms of its %.0f ms fetching is_open/e5/e10/diesel from table rows: %s stops at (station_id, recorded_at), so each of the %s matching rows costs a second lookup, %s. "+
			"An index carrying those four columns would make this read index-only, which is what idx_price_predictions_accuracy did for the accuracy page",
			lookups, q.DurationMS, indexOrPlan(q), formatCount(int64(q.Rows)),
			lookupRate(lookups, q.Rows)),
	})
	return findings
}

// dashboardPredictionFindings covers the two prediction reads. Both are bounded
// by station and fuel only, and the newest-run one is not bounded in time at
// all, so it walks every prediction stored for the scope inside the retention
// window to produce one row per station and fuel.
func dashboardPredictionFindings(byName map[string]doctorQuery, opts doctorOptions) []doctorFinding {
	var findings []doctorFinding

	if q, ok := byName["predictions_grid"]; ok && q.Error == "" && q.Rows > 0 {
		findings = append(findings, doctorFinding{
			Severity: "info",
			Message: fmt.Sprintf("predictions_grid returns %s future windows for the whole scope and PHP then reduces them to the newest run per station and fuel — the run filter is applied after the rows are transferred, not in SQL",
				formatCount(int64(q.Rows))),
		})
		if probe := q.Probe; probe != nil && probe.Error == "" && !q.CoveringHit {
			lookups := q.DurationMS - probe.DurationMS
			if severity, report := lookupSeverity(lookups, q.DurationMS, opts.SlowMS); report {
				findings = append(findings, doctorFinding{
					Severity: severity,
					Message: fmt.Sprintf("predictions_grid spends %.0f ms of its %.0f ms on the row lookups and the prediction_runs join that %s cannot satisfy, %s",
						lookups, q.DurationMS, indexOrPlan(q), lookupRate(lookups, q.Rows)),
				})
			}
		}
	}
	return findings
}

// writeDoctorDashboardText prints the dashboard section: the filters that were
// reproduced, how the scope narrowed, then one line per query with its probe
// indented beneath it.
func writeDoctorDashboardText(dash *doctorDashboard, opts doctorOptions, explain bool) {
	if dash == nil {
		return
	}
	if dash.Skipped {
		fmt.Fprintln(stdout, "\ndashboard queries:")
		for _, q := range dash.Queries {
			fmt.Fprintf(stdout, "  %-18s %s\n", q.Name, q.Purpose)
		}
		return
	}
	// Without a city there is no radius in play, so naming one would suggest a
	// filter the page never applied.
	scope := "city=(none)"
	if city := dash.Filters.City; city != "" {
		if dash.Filters.CityAuto {
			city += " (auto)"
		}
		scope = fmt.Sprintf("city=%s, radius=%d km", city, dash.Filters.RadiusKM)
	}
	upper := dash.Filters.To
	if upper == "" {
		upper = "now"
	}
	lower := dash.Filters.From
	if lower == "" {
		lower = "(unbounded)"
	}
	fmt.Fprintf(stdout, "\ndashboard queries: %s, fuel=%s (%s), %s .. %s\n",
		scope, dash.Filters.Fuel, strings.Join(dash.Fuels, "+"), lower, upper)
	if len(dash.Filters.Stations) > 0 {
		fmt.Fprintf(stdout, "  station picker: %d selected\n", len(dash.Filters.Stations))
	}
	fmt.Fprintf(stdout, "  scope: %d in the bounding box, %d within the radius, %d queried\n",
		dash.Scope.Candidates, dash.Scope.Stations, dash.Scope.Selected)

	for _, q := range dash.Queries {
		if q.Skipped {
			fmt.Fprintf(stdout, "  %-18s %s\n", q.Name, q.Purpose)
			continue
		}
		fmt.Fprintf(stdout, "  %-18s %9.1f ms%s %8d rows  %s\n",
			q.Name, q.DurationMS, spreadNote(q.DurationMS, q.SpreadMS), q.Rows, queryNote(q))
		writeDoctorProbeText(q.Probe, opts, explain)
		if opts.ShowSQL {
			fmt.Fprintf(stdout, "      sql: %s\n", q.SQL)
		}
		if explain {
			for _, line := range q.Plan {
				fmt.Fprintf(stdout, "      | %s\n", line)
			}
		}
	}
}

// queryNote is the one-word verdict at the end of a query's line.
func queryNote(q doctorQuery) string {
	switch {
	case q.Error != "":
		return "failed: " + q.Error
	case q.FullScan:
		return "TABLE SCAN"
	case q.UsesIndex != "" && q.CoveringHit:
		return "covering " + q.UsesIndex
	case q.UsesIndex != "":
		return q.UsesIndex
	}
	return "no index"
}

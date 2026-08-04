package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func stationAliasOf(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var alias sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT alias_of FROM stations WHERE id = ?`, id).Scan(&alias); err != nil {
		t.Fatalf("read alias_of for %s: %v", id, err)
	}
	return alias
}

func countSnapshots(t *testing.T, db *sql.DB, stationID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM price_snapshots WHERE station_id = ?`, stationID).Scan(&count); err != nil {
		t.Fatalf("count snapshots for %s: %v", stationID, err)
	}
	return count
}

func TestMergeStationsRewritesHistoryAndSetsAlias(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "canonical", "Kaiser-Tankstelle", 52.5, 13.4)
	insertSuggestStation(t, db, "dup-a", "Kaiser-Tankstelle Isenstedt", 52.5, 13.4)
	insertSuggestStation(t, db, "dup-b", "Kaiser-Tankstelle Isenstedt", 52.5, 13.4)

	day := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	insertSuggestSnapshot(t, db, "canonical", "Berlin", day.Add(8*time.Hour), 1.80, true)
	insertSuggestSnapshot(t, db, "dup-a", "Berlin", day.Add(9*time.Hour), 1.80, true)
	insertSuggestSnapshot(t, db, "dup-b", "Berlin", day.Add(10*time.Hour), 1.80, true)

	// One run scored all three identities of the same station for the same
	// target window; only dup-a's row got evaluated.
	runID := insertPredictionRunRow(t, db, day)
	insertPredictionRow(t, db, runID, "canonical", day.Add(12*time.Hour), 1.85, 60)
	evaluatedRow := insertPredictionRow(t, db, runID, "dup-a", day.Add(12*time.Hour), 1.85, 60)
	markPredictionEvaluated(t, db, evaluatedRow, 0.02, day.Add(14*time.Hour))
	insertPredictionRow(t, db, runID, "dup-b", day.Add(12*time.Hour), 1.85, 60)

	result, err := mergeStations(ctx, db, "canonical", []string{"dup-a", "dup-b"})
	if err != nil {
		t.Fatalf("mergeStations: %v", err)
	}
	if result.Snapshots != 2 || result.Predictions != 2 {
		t.Fatalf("moved snapshots/predictions = %d/%d, want 2/2", result.Snapshots, result.Predictions)
	}
	if result.PredictionsDeduped != 2 {
		t.Fatalf("predictions deduped = %d, want 2", result.PredictionsDeduped)
	}
	if countSnapshots(t, db, "canonical") != 3 || countSnapshots(t, db, "dup-a") != 0 {
		t.Fatalf("snapshot counts after merge: canonical=%d dup-a=%d, want 3/0",
			countSnapshots(t, db, "canonical"), countSnapshots(t, db, "dup-a"))
	}
	for _, dup := range []string{"dup-a", "dup-b"} {
		alias := stationAliasOf(t, db, dup)
		if !alias.Valid || alias.String != "canonical" {
			t.Fatalf("alias_of(%s) = %+v, want canonical", dup, alias)
		}
	}

	// A run that scored several identities of the same station leaves rows
	// that duplicate one logical measurement after the rewrite; the merge
	// must collapse them, keeping the evaluated row.
	var (
		kept   int
		keptID int64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(id) FROM price_predictions WHERE station_id = 'canonical'
	`).Scan(&kept, &keptID); err != nil {
		t.Fatalf("count predictions: %v", err)
	}
	if kept != 1 {
		t.Fatalf("predictions after merge = %d, want the duplicates collapsed to 1", kept)
	}
	if keptID != evaluatedRow {
		t.Fatalf("surviving prediction id = %d, want the evaluated row %d", keptID, evaluatedRow)
	}

	// Merging into an alias must be refused; merging an alias-of-alias must
	// re-point instead of chaining.
	if _, err := mergeStations(ctx, db, "dup-a", []string{"canonical"}); err == nil {
		t.Fatal("merging into an alias succeeded, want error")
	}
	insertSuggestStation(t, db, "dup-c", "Third identity", 52.5, 13.4)
	if _, err := mergeStations(ctx, db, "canonical", []string{"dup-c"}); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	alias := stationAliasOf(t, db, "dup-c")
	if !alias.Valid || alias.String != "canonical" {
		t.Fatalf("alias_of(dup-c) = %+v, want canonical", alias)
	}
}

func TestPersistSweepRedirectsAliasedStations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "canonical", "Kaiser-Tankstelle", 52.5, 13.4)
	insertSuggestStation(t, db, "dup-a", "Kaiser-Tankstelle Isenstedt", 52.5, 13.4)
	if _, err := mergeStations(ctx, db, "canonical", []string{"dup-a"}); err != nil {
		t.Fatalf("mergeStations: %v", err)
	}

	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.5, Lng: 13.4}
	insertSuggestCity(t, db, city)
	price := 1.79
	recordedAt := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	station := func(id string) tankerStation {
		return tankerStation{ID: id, Name: "Kaiser", Brand: "Kaiser", Lat: 52.5, Lng: 13.4, IsOpen: true, Diesel: &price}
	}
	observations := []stationObservation{
		{Station: station("canonical"), City: city, RadiusKM: 5, RecordedAt: recordedAt},
		{Station: station("dup-a"), City: city, RadiusKM: 5, RecordedAt: recordedAt},
	}
	if err := persistSweep(ctx, db, dialectSQLite, nil, observations); err != nil {
		t.Fatalf("persistSweep: %v", err)
	}

	// Both observations land on the canonical station, deduplicated to one
	// write; the alias id accrues no history of its own.
	if got := countSnapshots(t, db, "canonical"); got != 1 {
		t.Fatalf("canonical snapshots = %d, want 1", got)
	}
	if got := countSnapshots(t, db, "dup-a"); got != 0 {
		t.Fatalf("alias snapshots = %d, want 0", got)
	}

	// A sweep that only sees the alias still extends the canonical history.
	later := recordedAt.Add(time.Hour)
	lower := 1.75
	aliasOnly := station("dup-a")
	aliasOnly.Diesel = &lower
	if err := persistSweep(ctx, db, dialectSQLite, nil, []stationObservation{
		{Station: aliasOnly, City: city, RadiusKM: 5, RecordedAt: later},
	}); err != nil {
		t.Fatalf("persistSweep (alias only): %v", err)
	}
	if got := countSnapshots(t, db, "canonical"); got != 2 {
		t.Fatalf("canonical snapshots after alias-only sweep = %d, want 2", got)
	}
}

func insertStationWithAddress(t *testing.T, db *sql.DB, id, name, street string, lat, lng float64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO stations (id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at)
		VALUES (?, ?, 'TEST', ?, '1', 10115, 'Berlin', ?, ?, '2026-04-01T00:00:00Z', '2026-04-25T00:00:00Z')
	`, id, name, street, lat, lng); err != nil {
		t.Fatalf("insert station %s: %v", id, err)
	}
}

func TestDetectDuplicateStationsGroupsByCoordinates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertStationWithAddress(t, db, "a", "Station A", "Twin Street", 52.51730, 13.40010)
	insertStationWithAddress(t, db, "b", "Station B", "Twin Street", 52.51733, 13.40011) // same at 4 decimals
	insertStationWithAddress(t, db, "far", "Station Far", "Far Street", 52.60000, 13.50000)

	groups, err := detectDuplicateStations(ctx, db)
	if err != nil {
		t.Fatalf("detectDuplicateStations: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly one", groups)
	}
	if len(groups[0].Stations) != 2 {
		t.Fatalf("group size = %d, want 2", len(groups[0].Stations))
	}
	ids := map[string]bool{}
	for _, station := range groups[0].Stations {
		ids[station.ID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("grouped ids = %+v, want a and b", ids)
	}

	// Already-merged aliases stop appearing as candidates.
	if _, err := mergeStations(ctx, db, "a", []string{"b"}); err != nil {
		t.Fatalf("mergeStations: %v", err)
	}
	groups, err = detectDuplicateStations(ctx, db)
	if err != nil {
		t.Fatalf("detectDuplicateStations after merge: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups after merge = %+v, want none", groups)
	}
}

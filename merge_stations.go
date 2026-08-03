package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
)

// merge-stations deals with duplicate station identities: the Tankerkönig API
// sometimes lists the same physical station under several ids with identical
// prices. Duplicates triple-count every accuracy statistic and split the
// learned corrections' samples across ids, so they are worth merging.
//
// A merge rewrites the duplicates' price history (snapshots, predictions,
// check decisions) onto the canonical id and marks the duplicates with
// alias_of. The alias is honored at ingest (see persistSweep): future sweeps
// keep recording the duplicate ids' prices under the canonical station, so
// the merge is sticky even though the API keeps returning the old ids.

type mergeStationsResult struct {
	CanonicalID     string   `json:"canonical_id"`
	MergedIDs       []string `json:"merged_ids"`
	Snapshots       int64    `json:"snapshots"`
	Predictions     int64    `json:"predictions"`
	CheckDecisions  int64    `json:"check_decisions"`
	AliasesRepinned int64    `json:"aliases_repinned"`
}

type duplicateCandidate struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Brand   string  `json:"brand"`
	Address string  `json:"address"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
}

type duplicateGroup struct {
	Reason   string               `json:"reason"`
	Stations []duplicateCandidate `json:"stations"`
}

func runMergeStations(args []string) error {
	fs := flag.NewFlagSet("merge-stations", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	into := fs.String("into", "", "Canonical station id the duplicates are merged into")
	detect := fs.Bool("detect", false, "List duplicate-station candidates (same coordinates or same address) instead of merging")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := openDatabase(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initSchema(ctx, db, dbCfg.Driver); err != nil {
		return err
	}

	if *detect {
		if *into != "" || len(fs.Args()) > 0 {
			return errors.New("merge-stations --detect takes no further arguments")
		}
		groups, err := detectDuplicateStations(ctx, db)
		if err != nil {
			return err
		}
		if output == outputJSON {
			return writeJSON(groups)
		}
		if len(groups) == 0 {
			fmt.Fprintln(stdout, "no duplicate candidates found")
			return nil
		}
		for _, group := range groups {
			fmt.Fprintf(stdout, "duplicate candidates (%s):\n", group.Reason)
			for _, station := range group.Stations {
				fmt.Fprintf(stdout, "  %s  %s (%s) %s\n", station.ID, station.Name, station.Brand, station.Address)
			}
			fmt.Fprintf(stdout, "  merge with: gasoline merge-stations --into %s", group.Stations[0].ID)
			for _, station := range group.Stations[1:] {
				fmt.Fprintf(stdout, " %s", station.ID)
			}
			fmt.Fprintln(stdout)
		}
		return nil
	}

	canonical := strings.TrimSpace(*into)
	if canonical == "" {
		return errors.New("merge-stations requires --into <canonical-station-id> (or --detect)")
	}
	duplicates := make([]string, 0, len(fs.Args()))
	seen := map[string]bool{canonical: true}
	for _, arg := range fs.Args() {
		id := strings.TrimSpace(arg)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		duplicates = append(duplicates, id)
	}
	if len(duplicates) == 0 {
		return errors.New("merge-stations requires at least one duplicate station id distinct from --into")
	}

	result, err := mergeStations(ctx, db, canonical, duplicates)
	if err != nil {
		return err
	}
	if output == outputJSON {
		return writeJSON(result)
	}
	fmt.Fprintf(stdout, "merged %d station(s) into %s\n", len(result.MergedIDs), result.CanonicalID)
	fmt.Fprintf(stdout, "snapshots moved: %d\n", result.Snapshots)
	fmt.Fprintf(stdout, "predictions moved: %d\n", result.Predictions)
	fmt.Fprintf(stdout, "check decisions moved: %d\n", result.CheckDecisions)
	fmt.Fprintln(stdout, "future updates of the merged ids will record under the canonical station")
	fmt.Fprintln(stdout, "run `gasoline compact` to collapse overlapping snapshots from the merged histories")
	return nil
}

// mergeStations rewrites the duplicates' history onto the canonical station
// and marks them as aliases, all in one transaction.
func mergeStations(ctx context.Context, db *sql.DB, canonical string, duplicates []string) (mergeStationsResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mergeStationsResult{}, err
	}
	defer tx.Rollback()

	var canonicalAlias sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT alias_of FROM stations WHERE id = ?`, canonical).Scan(&canonicalAlias); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mergeStationsResult{}, fmt.Errorf("canonical station %q not found", canonical)
		}
		return mergeStationsResult{}, err
	}
	if canonicalAlias.Valid && canonicalAlias.String != "" {
		return mergeStationsResult{}, fmt.Errorf("canonical station %q is itself an alias of %q; merge into that instead", canonical, canonicalAlias.String)
	}

	result := mergeStationsResult{CanonicalID: canonical, MergedIDs: duplicates}
	for _, duplicate := range duplicates {
		var duplicateAlias sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT alias_of FROM stations WHERE id = ?`, duplicate).Scan(&duplicateAlias); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mergeStationsResult{}, fmt.Errorf("station %q not found", duplicate)
			}
			return mergeStationsResult{}, err
		}

		for _, move := range []struct {
			query string
			count *int64
		}{
			{`UPDATE price_snapshots SET station_id = ? WHERE station_id = ?`, &result.Snapshots},
			{`UPDATE price_predictions SET station_id = ? WHERE station_id = ?`, &result.Predictions},
			{`UPDATE price_check_decisions SET station_id = ? WHERE station_id = ?`, &result.CheckDecisions},
		} {
			exec, err := tx.ExecContext(ctx, move.query, canonical, duplicate)
			if err != nil {
				return mergeStationsResult{}, err
			}
			moved, err := exec.RowsAffected()
			if err != nil {
				return mergeStationsResult{}, err
			}
			*move.count += moved
		}

		if _, err := tx.ExecContext(ctx, `UPDATE stations SET alias_of = ? WHERE id = ?`, canonical, duplicate); err != nil {
			return mergeStationsResult{}, err
		}
		// Aliases already pointing at the duplicate must not become chains.
		exec, err := tx.ExecContext(ctx, `UPDATE stations SET alias_of = ? WHERE alias_of = ? AND id != ?`, canonical, duplicate, canonical)
		if err != nil {
			return mergeStationsResult{}, err
		}
		repinned, err := exec.RowsAffected()
		if err != nil {
			return mergeStationsResult{}, err
		}
		result.AliasesRepinned += repinned

		// The canonical station has now "been seen" for the union of both
		// histories.
		if _, err := tx.ExecContext(ctx, `
			UPDATE stations SET
				first_seen_at = CASE WHEN (SELECT s.first_seen_at FROM (SELECT first_seen_at FROM stations WHERE id = ?) s) < first_seen_at
					THEN (SELECT s.first_seen_at FROM (SELECT first_seen_at FROM stations WHERE id = ?) s) ELSE first_seen_at END,
				last_seen_at = CASE WHEN (SELECT s.last_seen_at FROM (SELECT last_seen_at FROM stations WHERE id = ?) s) > last_seen_at
					THEN (SELECT s.last_seen_at FROM (SELECT last_seen_at FROM stations WHERE id = ?) s) ELSE last_seen_at END
			WHERE id = ?
		`, duplicate, duplicate, duplicate, duplicate, canonical); err != nil {
			return mergeStationsResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return mergeStationsResult{}, err
	}
	return result, nil
}

// detectDuplicateStations groups non-alias stations that share (rounded)
// coordinates or a full street address. Both signals are strong for the
// duplicate-id case observed in practice: same pump, several ids, identical
// prices.
func detectDuplicateStations(ctx context.Context, db *sql.DB) ([]duplicateGroup, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, COALESCE(name_override, '') AS name_override, COALESCE(brand, '') AS brand,
			COALESCE(street, '') AS street, COALESCE(house_number, '') AS house_number,
			COALESCE(post_code, 0) AS post_code, COALESCE(place, '') AS place, lat, lng
		FROM stations
		WHERE alias_of IS NULL OR alias_of = ''
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type stationRecord struct {
		duplicateCandidate
		coordKey   string
		addressKey string
	}
	var stations []stationRecord
	for rows.Next() {
		var (
			record                                   stationRecord
			nameOverride, street, houseNumber, place string
			postCode                                 int
		)
		if err := rows.Scan(&record.ID, &record.Name, &nameOverride, &record.Brand,
			&street, &houseNumber, &postCode, &place, &record.Lat, &record.Lng); err != nil {
			return nil, err
		}
		if nameOverride != "" {
			record.Name = nameOverride
		}
		record.Address = strings.TrimSpace(fmt.Sprintf("%s %s, %d %s", street, houseNumber, postCode, place))
		// ~11 m at 4 decimal places: tighter than two distinct stations can
		// realistically sit, loose enough to absorb per-id jitter.
		record.coordKey = fmt.Sprintf("%.4f/%.4f", record.Lat, record.Lng)
		if street != "" && place != "" {
			record.addressKey = strings.ToLower(fmt.Sprintf("%s|%s|%d|%s", street, houseNumber, postCode, place))
		}
		stations = append(stations, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	grouped := make(map[string][]duplicateCandidate)
	reasons := make(map[string]string)
	for _, station := range stations {
		coordKey := "coords:" + station.coordKey
		grouped[coordKey] = append(grouped[coordKey], station.duplicateCandidate)
		reasons[coordKey] = "same coordinates"
		if station.addressKey != "" {
			addressKey := "address:" + station.addressKey
			grouped[addressKey] = append(grouped[addressKey], station.duplicateCandidate)
			reasons[addressKey] = "same address"
		}
	}

	keys := make([]string, 0, len(grouped))
	for key, members := range grouped {
		if len(members) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var groups []duplicateGroup
	emitted := make(map[string]bool)
	for _, key := range keys {
		members := grouped[key]
		// A coordinate group and an address group usually name the same set;
		// emit each set once.
		ids := make([]string, len(members))
		for i, member := range members {
			ids[i] = member.ID
		}
		signature := strings.Join(ids, ",")
		if emitted[signature] {
			continue
		}
		emitted[signature] = true
		groups = append(groups, duplicateGroup{Reason: reasons[key], Stations: members})
	}
	return groups, nil
}

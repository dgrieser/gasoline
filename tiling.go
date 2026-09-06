package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

const (
	// maxAPIRadiusKM is the largest radius Tankerkönig's /list.php serves. A
	// wider search has to be assembled from several queries of at most this
	// size — see planSearchTiles.
	maxAPIRadiusKM = 25.0
	// maxRequestRadiusKM is the largest radius --radius and an update target
	// accept. Everything between maxAPIRadiusKM and this is tiled.
	//
	// 42 km is where a bare ring of six reaches, and six requests is what the
	// schedule can afford with room for retries left over. The ceiling was 50,
	// which costs eight: seven disks of this size cover exactly twice their own
	// radius and no more, so 50 km sits a hair beyond what seven can do and
	// eight is the next stop. Those two extra requests bought 8 km of rim and
	// cost every sweep the headroom it needed when the API refused.
	maxRequestRadiusKM = 42.0
	// tilePlacementSafety shrinks the radius the tile geometry is *placed*
	// against, while every tile still requests the full maxAPIRadiusKM. The
	// difference is the margin that absorbs the projection's second-order
	// error and Tankerkönig's own distance rounding, so a station sitting on a
	// seam is returned by at least one tile instead of by none.
	tilePlacementSafety = 0.97

	// earthRadiusKM is the sphere every distance and bearing in this program is
	// measured on (IUGG mean radius).
	earthRadiusKM = 6371.0088

	// defaultRequestDelay and defaultRequestBurst are the pace a tiled sweep
	// keeps: at most defaultRequestBurst requests inside any window of
	// defaultRequestDelay.
	//
	// One request per window rather than a burst of them is what a shared key
	// tolerates best: the same budget spent evenly instead of three requests
	// arriving back to back and then a minute of silence.
	//
	// 50 s apart, flat. Tankerkönig answers a paced sweep with 503s often
	// enough that retries are routine rather than exceptional, and a retry is
	// the API asking to be left alone — so the window is set as wide as the
	// schedule allows rather than as narrow as the sweep needs. At six requests
	// that is 4:10 of the 4:50 a five-minute schedule affords.
	//
	// It replaces a 37 s window with an extra two seconds after every third
	// request — an arrangement that bought a 1.7% lower request rate and three
	// shapes of pacing to reason about instead of one.
	defaultRequestDelay = 50 * time.Second
	defaultRequestBurst = 1

	// sweepBudget is the wall clock a tiled sweep has to fit inside. The
	// packaged cron entry and systemd timer fire `update` every five minutes
	// and lean on flock to drop a run that would overlap the last one, so a
	// sweep that overruns does not queue up — it loses the whole cycle. The ten
	// seconds held back cover geocoding, the requests themselves and the write
	// at the end.
	//
	// The widest sweep is a 42 km target: 6 requests, the last going out at
	// 4:10. What is left of the budget after that is sweepRetryHeadroom.
	sweepBudget = 4*time.Minute + 50*time.Second

	// sweepRetryHeadroom is how many retries the widest sweep can absorb and
	// still finish inside the budget, and at this pace it is none: a seventh
	// request lands at 5:00, ten seconds past. That is the trade the window is
	// on the other side of — every second added to the pace is more room for
	// the API to be left alone and less room for it to be asked twice — and 48 s
	// is the widest window that still fits a retry, if the balance ever wants
	// moving back. A sweep that does retry finishes late and loses the next
	// cycle to flock, so prices land ten minutes apart rather than five.
	sweepRetryHeadroom = 0

	// minRingTiles is the smallest ring either construction is defined for.
	minRingTiles = 3

	// defaultTileRetries is how often one tile is retried after a failure that
	// retrying can plausibly fix, while nothing else says otherwise.
	defaultTileRetries = 1

	// The bounds an admin-supplied pace has to fall inside. They are wide
	// enough that nobody sane meets them and narrow enough that a typo in the
	// settings table cannot turn a sweep into an hour of sleeping or a burst
	// into a flood; a value outside them is ignored in favour of the default,
	// which is the only outcome that keeps sweeping.
	maxConfigurableRequestDelay = 10 * time.Minute
	maxConfigurableRequestBurst = 10
	maxConfigurableTileRetries  = 5

	// The outcome of one recorded request. A retried attempt is kept as its own
	// row rather than folded into the try that replaced it: the whole point of
	// recording attempts is that a tile which needed two of them cost a whole
	// pacing window, and a log that only shows the winner hides that.
	tileAttemptOK      = "ok"
	tileAttemptRetried = "retried"
	tileAttemptFailed  = "failed"

	// maxLoggedTileAttempts bounds what one sweep keeps. A sweep of a single
	// 42 km target is 6 requests, 12 in the worst retry case, so this is only
	// reached by a sweep with dozens of tiled targets — where the newest
	// attempts are the ones worth having and the storage is better spent than
	// on a run row with thousands of children.
	maxLoggedTileAttempts = 512
)

// sleepFn and nowFn are the clock the request pacing runs on, replaced in tests
// so a spacing assertion neither sleeps nor races.
var (
	sleepFn = time.Sleep
	nowFn   = time.Now
)

// searchTile is one Tankerkönig /list.php query: a centre and the radius to ask
// for. A tiled search issues several of these and merges the answers.
type searchTile struct {
	Lat      float64
	Lng      float64
	RadiusKM float64
}

// ringReach returns the distance to place a ring of n tiles at, and how far out
// from the centre that ring then reaches, for tiles of placement radius rd.
//
// A ring of n tiles at distance d covers the annulus
// d·cos φ ± √(rd² − d²·sin²φ), where φ = π/n is the half-angle between
// neighbours: the worst-covered direction is the one halfway between two tiles.
// Two things bound d. Pushing the ring outwards reaches further until
// d = rd·cot φ, where the outer reach peaks at rd/sin φ. But the ring also has
// to still meet the centre tile, which reaches rd; its inner edge touches rd
// exactly at d = 2·rd·cos φ. Whichever bound bites first wins, and they cross
// at sin φ = ½ — that is, at n = 6.
func ringReach(n int, rd float64) (d, outer float64) {
	phi := math.Pi / float64(n)
	sin, cos := math.Sin(phi), math.Cos(phi)
	if sin >= 0.5 { // n <= 6: the outer reach peaks before the centre tile is left behind
		return rd * cos / sin, rd / sin
	}
	// n >= 7: held back to keep the ring's inner edge on the centre tile.
	return 2 * rd * cos, rd * (2*cos*cos + math.Cos(2*phi))
}

// bareRingReach returns the distance to place a ring of n tiles at, and how far
// out it then reaches, when there is no centre tile under it.
//
// Without one, the ring itself has to cover the middle, which caps d at rd. Left
// free the best distance is rd·cot φ, reaching rd/sin φ; that stays inside the
// cap while cot φ ≤ 1, which is n ≤ 4. Beyond that the ring is held at d = rd
// and reaches 2·rd·cos φ.
//
// This is what the centre tile costs. For n ≤ 4 a ring reaches exactly as far
// without one as with one, so the centre tile is a whole request spent on
// ground the ring already covers. For 5 and 6 the ring pulled in to d = rd
// still reaches further than a centre tile plus a ring one smaller. Only from
// 7 does the centre tile start earning its request, and there it earns it
// handsomely — the hexagon reaches 2·rd, which no bare ring approaches.
func bareRingReach(n int, rd float64) (d, outer float64) {
	phi := math.Pi / float64(n)
	sin, cos := math.Sin(phi), math.Cos(phi)
	if cos <= sin { // n <= 4: the unconstrained best still covers the middle
		return rd * cos / sin, rd / sin
	}
	return rd, 2 * rd * cos
}

// planSearchTiles covers the disk of radiusKM around (lat, lng) with tiles no
// larger than the API's own limit, leaving no gaps.
//
// A radius the API can serve directly returns exactly one tile asking for
// exactly that radius, so nothing about a narrow search changes. Anything wider
// becomes the cheapest ring that reaches far enough — a bare ring up to six
// requests, a ring around a centre tile beyond that (see bareRingReach and
// ringReach). The tile order is fixed, which keeps the merge deterministic.
func planSearchTiles(lat, lng, radiusKM float64) ([]searchTile, error) {
	if radiusKM <= 0 {
		return nil, fmt.Errorf("radius %.2f km must be positive", radiusKM)
	}
	if radiusKM <= maxAPIRadiusKM {
		return []searchTile{{Lat: lat, Lng: lng, RadiusKM: radiusKM}}, nil
	}
	if radiusKM > maxRequestRadiusKM {
		return nil, fmt.Errorf("radius %.2f km exceeds the supported maximum of %.0f km", radiusKM, maxRequestRadiusKM)
	}

	// Both constructions are tried at every request count, smallest count
	// first, and the first that reaches far enough wins. Neither is better
	// everywhere: a bare ring is the whole of it up to six requests, a centre
	// tile and a ring from seven.
	// Three is where both constructions start: two tiles on a ring are two
	// tiles on top of each other, and a "ring" of one or two under a centre
	// tile is not a ring at all — the closed forms below describe neither.
	rd := maxAPIRadiusKM * tilePlacementSafety
	for requests := minRingTiles; requests <= 13; requests++ {
		// A bare ring of `requests` tiles.
		if d, outer := bareRingReach(requests, rd); outer >= radiusKM {
			return ringTiles(lat, lng, d, requests, false), nil
		}
		// A centre tile and a ring of one fewer.
		if requests > minRingTiles {
			if d, outer := ringReach(requests-1, rd); outer >= radiusKM {
				return ringTiles(lat, lng, d, requests-1, true), nil
			}
		}
	}
	// Unreachable while maxRequestRadiusKM stays inside these constructions'
	// reach; a raised ceiling has to grow them rather than silently
	// under-cover the disk.
	return nil, fmt.Errorf("no tiling covers a radius of %.2f km", radiusKM)
}

// ringTiles lays n tiles evenly around (lat, lng) at distance d, optionally
// preceded by one on the centre. Tile 0 is the centre when there is one and
// otherwise the first of the ring, which keeps the merge order deterministic
// either way.
func ringTiles(lat, lng, d float64, n int, centre bool) []searchTile {
	tiles := make([]searchTile, 0, n+1)
	if centre {
		tiles = append(tiles, searchTile{Lat: lat, Lng: lng, RadiusKM: maxAPIRadiusKM})
	}
	for k := 0; k < n; k++ {
		tileLat, tileLng := destinationPoint(lat, lng, d, float64(k)*360/float64(n))
		tiles = append(tiles, searchTile{Lat: tileLat, Lng: tileLng, RadiusKM: maxAPIRadiusKM})
	}
	return tiles
}

// destinationPoint returns the point distanceKM away from (lat, lng) along
// bearingDeg, measured clockwise from true north.
//
// This is the exact spherical construction, not a flat approximation: it keeps
// the distance from the centre exact at every bearing and latitude, which is
// what the ring geometry and the overshoot filter both measure.
func destinationPoint(lat, lng, distanceKM, bearingDeg float64) (float64, float64) {
	delta := distanceKM / earthRadiusKM
	theta := degreesToRadians(bearingDeg)
	latRad := degreesToRadians(lat)
	lngRad := degreesToRadians(lng)

	sinLat := math.Sin(latRad)*math.Cos(delta) + math.Cos(latRad)*math.Sin(delta)*math.Cos(theta)
	destLat := math.Asin(sinLat)
	destLng := lngRad + math.Atan2(
		math.Sin(theta)*math.Sin(delta)*math.Cos(latRad),
		math.Cos(delta)-math.Sin(latRad)*sinLat,
	)
	return destLat * 180 / math.Pi, normalizeLongitude(destLng * 180 / math.Pi)
}

// normalizeLongitude folds a longitude back into [-180, 180), so a tile placed
// across the antimeridian stays a valid coordinate.
func normalizeLongitude(lng float64) float64 {
	return math.Mod(math.Mod(lng+180, 360)+360, 360) - 180
}

// tileAttempt is one Tankerkönig request as it actually went out: which tile of
// which city it was for, which try, when the pacing released it, how long the
// pacing had held it, and what the request itself then cost.
//
// Waited and the duration are kept apart on purpose. They are the two halves of
// a tiled sweep's wall clock and they have opposite fixes: time spent waiting is
// the pace we chose and is tuned with --request-delay, while time spent inside
// the request is the API being slow and is not ours to tune. A single elapsed
// figure per tile would leave the admin page unable to tell them apart.
type tileAttempt struct {
	City     string
	Tile     int // the tile's index in the plan; 0 is the first one queried
	Attempt  int // 1 for the first try, 2 for the retry
	SentAt   time.Time
	Waited   time.Duration
	Duration time.Duration
	Status   string
	Err      string
}

// tileLog collects a sweep's requests in the order they went out. A nil log
// records nothing, which is what a caller that only wants the stations passes.
//
// The totals are accumulated separately from the list rather than derived from
// it, because only the list is capped: a sweep of dozens of tiled targets stops
// keeping individual requests long before it stops counting them, and a
// request-count metric that silently stopped at the cap would be the one number
// on the page nobody could sanity-check.
type tileLog struct {
	attempts []tileAttempt
	total    int
	retries  int
	slowest  time.Duration
	waited   time.Duration
}

// record counts one request and, while there is room, keeps it. The city is
// filled in by the caller that knows it: the tiling itself only ever sees
// coordinates.
func (l *tileLog) record(a tileAttempt) {
	if l == nil {
		return
	}
	l.total++
	if a.Attempt > 1 {
		l.retries++
	}
	if a.Duration > l.slowest {
		l.slowest = a.Duration
	}
	l.waited += a.Waited
	if len(l.attempts) >= maxLoggedTileAttempts {
		return
	}
	l.attempts = append(l.attempts, a)
}

// dropped is how many requests were counted but not kept.
func (l *tileLog) dropped() int {
	if l == nil {
		return 0
	}
	return l.total - len(l.attempts)
}

// nameCity labels every attempt recorded since the log held from attempts
// requests with the city they belong to. The fetch that knows the city name
// brackets its own tiles with it, which keeps the city out of every function
// signature between here and there.
func (l *tileLog) nameCity(from int, city string) {
	if l == nil {
		return
	}
	for i := from; i < len(l.attempts); i++ {
		l.attempts[i].City = city
	}
}

// count is how many attempts the log holds, which is also where the next one
// will land — a caller brackets a city with it.
func (l *tileLog) count() int {
	if l == nil {
		return 0
	}
	return len(l.attempts)
}

// tankerPacing is how a sweep talks to Tankerkönig: how fast it asks, and how
// insistently. At most burst requests go out inside any window of delay —
// request i waits until request i-burst is delay old, so a burst goes out back
// to back and the one after it waits for the window to roll over. A zero delay
// or burst never waits at all, which is what a sweep with nothing to tile gets.
//
// retries belongs here rather than beside the request that uses it because it
// is the same trade the window is: both decide how much of the schedule the
// sweep spends being polite to an API that is refusing, and an admin moves them
// together (see resolveTankerPacing).
type tankerPacing struct {
	delay   time.Duration
	burst   int
	retries int
	// recent holds the times of the last burst requests, oldest first.
	recent []time.Time
}

// wait blocks until this request's turn in the pace comes up, and returns the
// instant it goes out. That is not necessarily the instant wait was called: a
// caller whose window is already full is held back, so anything that wants to
// record when a request was actually made has to take it from here.
func (l *tankerPacing) wait() time.Time {
	if l == nil || l.delay <= 0 || l.burst < 1 {
		return nowFn()
	}
	now := nowFn()
	if len(l.recent) >= l.burst {
		if until := l.recent[0].Add(l.delay); until.After(now) {
			sleepFn(until.Sub(now))
			// Book the slot at the time we waited for rather than re-reading
			// the clock: the pace then stays even instead of drifting by
			// however long each request took.
			now = until
		}
		l.recent = l.recent[1:]
	}
	l.recent = append(l.recent, now)
	return now
}

// tankerPacingFlags is what the command line said about the pace, and whether
// it said anything at all. The zero-set flags are what makes "the admin setting
// is the default" expressible: --request-delay 50s typed out has to mean the
// same thing as leaving it off only when the setting also says 50s.
type tankerPacingFlags struct {
	delay      time.Duration
	burst      int
	retries    int
	delaySet   bool
	burstSet   bool
	retriesSet bool
}

// resolveTankerPacing settles the pace for one sweep. The admin setting is the
// default and an explicit flag overrides it, one value at a time: an operator
// slowing a single run down from the command line should not silently take the
// stored burst or retry count back to the built-in one.
//
// The delay is armed only when something in the sweep actually needs tiling.
// The pace is a property of the API key, not of the city, so once anything here
// is tiled every request the sweep makes is spaced — including the seam between
// one city's last tile and the next city's first — but a sweep where every
// target fits in one request waits for nothing. Retries are not gated that way:
// a single-request city that fails is worth asking again for the same reason a
// tile is.
func resolveTankerPacing(s appSettings, f tankerPacingFlags, queries []cityQuery) *tankerPacing {
	pace := &tankerPacing{delay: s.RequestDelay, burst: s.RequestBurst, retries: s.TileRetries}
	if f.delaySet {
		pace.delay = f.delay
	}
	if f.burstSet {
		pace.burst = f.burst
	}
	if f.retriesSet {
		pace.retries = f.retries
	}
	tiled := false
	for _, q := range queries {
		if q.radius > maxAPIRadiusKM {
			tiled = true
			break
		}
	}
	if !tiled {
		pace.delay = 0
	}
	return pace
}

// retryBudget is how many extra attempts one tile gets, and it is exactly what
// the pace says: there is no "unset" reading of it, because an admin asking for
// no retries at all is a real answer this has to be able to give. Every pace
// that reaches a request is built by resolveTankerPacing, which is where the
// default lives.
func (l *tankerPacing) retryBudget() int {
	if l == nil || l.retries < 0 {
		return 0
	}
	return l.retries
}

// pace reports how long this limiter takes to let n requests out, measured from
// the first — request i goes out at (i/burst)·delay, so the last one waits
// ((n-1)/burst)·delay. It is what sizes the defaults against sweepBudget.
func (l *tankerPacing) pace(n int) time.Duration {
	if l == nil || l.delay <= 0 || l.burst < 1 || n < 2 {
		return 0
	}
	return time.Duration((n-1)/l.burst) * l.delay
}

// tankerRequestError carries whether retrying a failed Tankerkönig request can
// plausibly help. It reports the wrapped error's own message unchanged, so the
// text a failed sweep prints does not depend on how it was classified.
type tankerRequestError struct {
	err       error
	retryable bool
}

func (e *tankerRequestError) Error() string { return e.err.Error() }
func (e *tankerRequestError) Unwrap() error { return e.err }

// retryableTankerError reports whether err is a Tankerkönig failure worth one
// more attempt. Anything unclassified counts as permanent: a retry costs a
// whole pacing window, so it has to be earned.
func retryableTankerError(err error) bool {
	var reqErr *tankerRequestError
	if errors.As(err, &reqErr) {
		return reqErr.retryable
	}
	return false
}

// retryableStatus reports whether an HTTP status says "later, maybe" rather
// than "no".
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// fetchTiledStations queries every tile, paced by lim, and returns the union of
// what they reported: one entry per station id, restricted to radiusKM of the
// centre, with Dist restated as the distance from that centre. It also reports
// when the first request went out, which the caller stamps the whole city with.
//
// The first tile is treated as the load-bearing one: if it fails even on retry
// the whole city fails, because the cause is almost always systemic (a rejected
// key, no network) and the alternative is reporting a city assembled out of
// whatever happened to answer. That also makes it the first
// request that answered, so it is the one observedAt comes from. A later tile
// failing costs only the stations that tile alone could see, which is a gap the
// next sweep closes well inside the 48-hour freshness window — so those are
// counted and the city is still stored.
func fetchTiledStations(ctx context.Context, cfg config, lim *tankerPacing, log *tileLog, centre cachedCity, tiles []searchTile, radiusKM float64, fuelType, sortBy string) ([]tankerStation, time.Time, int, error) {
	merged := make(map[string]tankerStation)
	order := make([]string, 0, len(tiles)*64)
	tilesFailed := 0
	var observedAt time.Time

	for i, tile := range tiles {
		stations, sentAt, err := fetchTileStations(ctx, cfg, lim, log, i, tile, fuelType, sortBy)
		if err != nil {
			if i == 0 {
				return nil, time.Time{}, 0, err
			}
			tilesFailed++
			continue
		}
		if i == 0 {
			observedAt = sentAt
		}
		for _, station := range stations {
			if _, seen := merged[station.ID]; seen {
				// The tiles overlap by construction, so most stations arrive
				// more than once. The first tile to report one wins, and the
				// tile order is fixed, so the choice is deterministic.
				continue
			}
			merged[station.ID] = station
			order = append(order, station.ID)
		}
	}

	// A single tile is the API's own answer to the API's own radius: it already
	// filtered, sorted and measured from the point we asked about, so it is
	// passed through untouched.
	if len(tiles) == 1 {
		stations := make([]tankerStation, 0, len(order))
		for _, id := range order {
			stations = append(stations, merged[id])
		}
		return stations, observedAt, tilesFailed, nil
	}

	stations := make([]tankerStation, 0, len(order))
	for _, id := range order {
		station := merged[id]
		distance := haversineKM(centre.Lat, centre.Lng, station.Lat, station.Lng)
		// The union of overlapping tiles bulges past the radius that was asked
		// for. Keeping the bulge would store stations that the ownership rules
		// then measure as out of reach, handing them back and forth between
		// sweeps, so the disk is trimmed to the radius the caller named.
		if distance > radiusKM {
			continue
		}
		// Dist arrived measured from whichever tile reported the station.
		// Restate it from the city centre, which is what it means everywhere
		// else in the program.
		station.Dist = math.Round(distance*1000) / 1000
		stations = append(stations, station)
	}
	sortStations(stations, fuelType, sortBy)
	return stations, observedAt, tilesFailed, nil
}

// fetchTileStations fetches one tile, waiting for its slot in the pace first and
// retrying once if the failure looks transient. It reports when the attempt that
// answered went out: an attempt that failed saw nothing, so a retried tile is
// anchored to the retry rather than to the try that came back empty.
func fetchTileStations(ctx context.Context, cfg config, lim *tankerPacing, log *tileLog, index int, tile searchTile, fuelType, sortBy string) ([]tankerStation, time.Time, error) {
	var err error
	for attempt := 0; attempt <= lim.retryBudget(); attempt++ {
		queued := nowFn()
		sentAt := lim.wait()
		var stations []tankerStation
		stations, err = fetchStations(ctx, cfg, tile.Lat, tile.Lng, tile.RadiusKM, fuelType, sortBy)
		// Every attempt is recorded before it is acted on, so the log describes
		// the requests that were made rather than the ones that worked. The
		// status is decided here because only this loop knows whether a failure
		// is about to be retried or is the tile's last word.
		record := tileAttempt{
			Tile:     index,
			Attempt:  attempt + 1,
			SentAt:   sentAt,
			Waited:   sentAt.Sub(queued),
			Duration: nowFn().Sub(sentAt),
			Status:   tileAttemptOK,
		}
		if err != nil {
			record.Status = tileAttemptFailed
			record.Err = err.Error()
			if attempt < lim.retryBudget() && retryableTankerError(err) && ctx.Err() == nil {
				record.Status = tileAttemptRetried
			}
		}
		log.record(record)
		if err == nil {
			return stations, sentAt, nil
		}
		if !retryableTankerError(err) {
			return nil, time.Time{}, err
		}
		if ctx.Err() != nil {
			return nil, time.Time{}, err
		}
	}
	return nil, time.Time{}, err
}

// sortStations restores the order a single fetch would have come back in: each
// tile sorted its own answer relative to its own centre, so a merged list has
// to be put back in order relative to the city.
func sortStations(stations []tankerStation, fuelType, sortBy string) {
	if sortBy == "price" && fuelType != "all" {
		sort.SliceStable(stations, func(i, j int) bool {
			a, b := stationPrice(stations[i], fuelType), stationPrice(stations[j], fuelType)
			switch {
			case a == nil && b == nil:
				return stations[i].ID < stations[j].ID
			case a == nil:
				return false // a station without a price for this fuel sorts last
			case b == nil:
				return true
			case *a != *b:
				return *a < *b
			}
			return stations[i].ID < stations[j].ID
		})
		return
	}
	sort.SliceStable(stations, func(i, j int) bool {
		if stations[i].Dist != stations[j].Dist {
			return stations[i].Dist < stations[j].Dist
		}
		return stations[i].ID < stations[j].ID
	})
}

// stationPrice returns the station's price for one fuel, or nil when it has none.
func stationPrice(station tankerStation, fuelType string) *float64 {
	switch fuelType {
	case "diesel":
		return station.Diesel
	case "e5":
		return station.E5
	case "e10":
		return station.E10
	}
	return nil
}

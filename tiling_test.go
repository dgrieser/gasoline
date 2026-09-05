package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tileTestCentres spans the cases the projection could get wrong: mid-latitude
// (where the program is actually used), a high latitude where a degree of
// longitude is short, the equator, the antimeridian, and the southern
// hemisphere.
var tileTestCentres = []struct {
	name     string
	lat, lng float64
}{
	{"berlin", 52.5, 13.4},
	{"alps", 47.5, 9.0},
	{"arctic", 71.0, 25.0},
	{"antimeridian", 0.0, 179.9},
	{"southern", -33.9, 18.4},
}

// A radius the API serves directly must still be one request for exactly that
// radius: the whole point of the tiling is that it does not disturb the narrow
// searches everything already uses.
func TestPlanSearchTilesSingleFetch(t *testing.T) {
	for _, radius := range []float64{1, 5, 12.5, maxAPIRadiusKM} {
		tiles, err := planSearchTiles(52.5, 13.4, radius)
		if err != nil {
			t.Fatalf("radius %.2f: %v", radius, err)
		}
		if len(tiles) != 1 {
			t.Fatalf("radius %.2f: %d tiles, want 1", radius, len(tiles))
		}
		if tiles[0].Lat != 52.5 || tiles[0].Lng != 13.4 {
			t.Fatalf("radius %.2f: tile at %.6f,%.6f, want the city centre", radius, tiles[0].Lat, tiles[0].Lng)
		}
		if tiles[0].RadiusKM != radius {
			t.Fatalf("radius %.2f: tile asks for %.2f", radius, tiles[0].RadiusKM)
		}
	}
}

// The load-bearing test: the tiles must leave no gap anywhere in the disk that
// was asked for. Sampling in real lat/lng and measuring with haversineKM means
// this covers the projection too, not just the planar geometry.
func TestPlanSearchTilesCoversDisk(t *testing.T) {
	// Every sample has to sit inside some tile with room to spare. The margin
	// exists because Tankerkönig applies its own radius filter with its own
	// distance calculation, so a station measured at exactly the radius is not
	// reliably returned.
	const wantWithinKM = 24.30

	radii := []float64{25.5, 26, 28, 30, 34, 34.5, 35, 40, 41, 41.5, 42, 45, 48, 48.6, 49, 50}
	for _, centre := range tileTestCentres {
		for _, radius := range radii {
			tiles, err := planSearchTiles(centre.lat, centre.lng, radius)
			if err != nil {
				t.Fatalf("%s r=%.2f: %v", centre.name, radius, err)
			}
			worst, worstLat, worstLng := worstUncoveredPoint(centre.lat, centre.lng, radius, tiles)
			if worst > wantWithinKM {
				t.Errorf("%s r=%.2f (%d tiles): point %.6f,%.6f is %.3f km from the nearest tile centre, want <= %.2f",
					centre.name, radius, len(tiles), worstLat, worstLng, worst, wantWithinKM)
			}
		}
	}
}

// worstUncoveredPoint returns the point of the disk furthest from every tile
// centre, and that distance. A polar sweep is the natural grid here: the tiling
// is built out of rings, so the directions between two neighbouring tiles are
// where a gap would open.
func worstUncoveredPoint(lat, lng, radiusKM float64, tiles []searchTile) (worst, worstLat, worstLng float64) {
	const (
		radialSteps  = 120
		angularSteps = 720
	)
	for i := 0; i <= radialSteps; i++ {
		rho := radiusKM * float64(i) / radialSteps
		for k := 0; k < angularSteps; k++ {
			bearing := float64(k) * 360 / angularSteps
			sampleLat, sampleLng := destinationPoint(lat, lng, rho, bearing)
			nearest := math.Inf(1)
			for _, tile := range tiles {
				if d := haversineKM(tile.Lat, tile.Lng, sampleLat, sampleLng); d < nearest {
					nearest = d
				}
			}
			if nearest > worst {
				worst, worstLat, worstLng = nearest, sampleLat, sampleLng
			}
		}
	}
	return worst, worstLat, worstLng
}

// The tile count is the API request budget, so it is pinned: a geometry change
// that quietly doubles the traffic a sweep generates should fail here.
func TestPlanSearchTilesTileCount(t *testing.T) {
	cases := []struct {
		radius float64
		want   int
	}{
		{5, 1}, {25, 1},
		{25.5, 4}, {28, 4},
		{30, 5}, {34, 5},
		{35, 6}, {41, 6},
		{42, 7}, {48, 7},
		{49, 8}, {50, 8},
	}
	for _, tc := range cases {
		tiles, err := planSearchTiles(52.5, 13.4, tc.radius)
		if err != nil {
			t.Fatalf("radius %.2f: %v", tc.radius, err)
		}
		if len(tiles) != tc.want {
			t.Errorf("radius %.2f: %d tiles, want %d", tc.radius, len(tiles), tc.want)
		}
	}
}

// Across the whole tiled range the plan has to stay well formed: never cheaper
// for a wider disk, always centred on the city, always asking the API for the
// most it will serve, and always one ring of equidistant tiles.
func TestPlanSearchTilesLadder(t *testing.T) {
	previous := 0
	for radius := 25.5; radius <= maxRequestRadiusKM+1e-9; radius += 0.5 {
		tiles, err := planSearchTiles(52.5, 13.4, radius)
		if err != nil {
			t.Fatalf("radius %.2f: %v", radius, err)
		}
		if len(tiles) < previous {
			t.Fatalf("radius %.2f: %d tiles, fewer than the %d a smaller radius took", radius, len(tiles), previous)
		}
		previous = len(tiles)

		if tiles[0].Lat != 52.5 || tiles[0].Lng != 13.4 {
			t.Fatalf("radius %.2f: tile 0 is not the city centre", radius)
		}
		for i, tile := range tiles {
			if tile.RadiusKM != maxAPIRadiusKM {
				t.Fatalf("radius %.2f: tile %d asks for %.2f km, want the API maximum", radius, i, tile.RadiusKM)
			}
		}
		// One ring: every tile but the centre sits at the same distance.
		ring := haversineKM(52.5, 13.4, tiles[1].Lat, tiles[1].Lng)
		for i, tile := range tiles[1:] {
			d := haversineKM(52.5, 13.4, tile.Lat, tile.Lng)
			if math.Abs(d-ring) > 1e-6 {
				t.Fatalf("radius %.2f: ring tile %d is %.6f km out, want %.6f", radius, i+1, d, ring)
			}
		}
	}
}

func TestPlanSearchTilesDeterministic(t *testing.T) {
	first, err := planSearchTiles(52.5, 13.4, 50)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := planSearchTiles(52.5, 13.4, 50)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("%d tiles then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("tile %d: %+v then %+v", i, first[i], second[i])
		}
	}
}

func TestPlanSearchTilesRejectsOutOfRange(t *testing.T) {
	if _, err := planSearchTiles(52.5, 13.4, 0); err == nil {
		t.Fatal("expected an error for a zero radius")
	}
	if _, err := planSearchTiles(52.5, 13.4, maxRequestRadiusKM+0.01); err == nil {
		t.Fatal("expected an error for a radius past the supported maximum")
	}
}

// ringReach is the whole geometry in one function, so its two branches and the
// point they cross at are worth stating outright.
func TestRingReach(t *testing.T) {
	rd := maxAPIRadiusKM * tilePlacementSafety

	// n <= 6: the ring sits at rd*cot(phi) and the outer reach peaks at rd/sin(phi).
	d, outer := ringReach(6, rd)
	if want := rd * math.Sqrt(3); math.Abs(d-want) > 1e-9 { // cot(30 deg) = sqrt(3)
		t.Fatalf("n=6: ring at %.6f, want %.6f", d, want)
	}
	if want := rd * 2; math.Abs(outer-want) > 1e-9 { // 1/sin(30 deg) = 2
		t.Fatalf("n=6: reaches %.6f, want %.6f", outer, want)
	}

	// n >= 7: held back to 2*rd*cos(phi) so the ring still meets the centre tile.
	phi := math.Pi / 7
	d, outer = ringReach(7, rd)
	if math.Abs(d-2*rd*math.Cos(phi)) > 1e-9 {
		t.Fatalf("n=7: ring at %.6f, want %.6f", d, 2*rd*math.Cos(phi))
	}
	want := rd * (2*math.Cos(phi)*math.Cos(phi) + math.Cos(2*phi))
	if math.Abs(outer-want) > 1e-9 {
		t.Fatalf("n=7: reaches %.6f, want %.6f", outer, want)
	}

	// More tiles never reach less far.
	previous := 0.0
	for n := 3; n <= 12; n++ {
		_, outer := ringReach(n, rd)
		if outer <= previous {
			t.Fatalf("n=%d reaches %.6f, not past the %.6f of one tile fewer", n, outer, previous)
		}
		previous = outer
	}

	// Every ring's inner edge has to reach the centre tile, or the two leave a
	// hole between them.
	for n := 3; n <= 12; n++ {
		d, _ := ringReach(n, rd)
		phi := math.Pi / float64(n)
		perp := d * math.Sin(phi)
		if perp >= rd {
			t.Fatalf("n=%d: neighbouring tiles %.6f km apart cannot overlap", n, perp)
		}
		inner := d*math.Cos(phi) - math.Sqrt(rd*rd-perp*perp)
		if inner > rd+1e-9 {
			t.Fatalf("n=%d: ring starts at %.6f km but the centre tile ends at %.6f", n, inner, rd)
		}
	}
}

func TestDestinationPoint(t *testing.T) {
	for _, centre := range tileTestCentres {
		for _, dist := range []float64{1, 25, 50, 1000} {
			for bearing := 0.0; bearing < 360; bearing += 45 {
				lat, lng := destinationPoint(centre.lat, centre.lng, dist, bearing)
				got := haversineKM(centre.lat, centre.lng, lat, lng)
				if math.Abs(got-dist) > 1e-6 {
					t.Errorf("%s d=%.0f b=%.0f: landed %.9f km out", centre.name, dist, bearing, got)
				}
				if lng < -180 || lng >= 180 {
					t.Errorf("%s d=%.0f b=%.0f: longitude %.6f is out of range", centre.name, dist, bearing, lng)
				}
				if lat < -90 || lat > 90 {
					t.Errorf("%s d=%.0f b=%.0f: latitude %.6f is out of range", centre.name, dist, bearing, lat)
				}
			}
		}
	}

	// Due north raises the latitude and leaves the longitude alone; due east
	// raises the longitude and (near the equator) barely moves the latitude.
	north, northLng := destinationPoint(0, 0, 100, 0)
	if north <= 0 || math.Abs(northLng) > 1e-9 {
		t.Fatalf("due north landed at %.6f,%.6f", north, northLng)
	}
	eastLat, east := destinationPoint(0, 0, 100, 90)
	if east <= 0 || math.Abs(eastLat) > 1e-9 {
		t.Fatalf("due east landed at %.6f,%.6f", eastLat, east)
	}
}

func TestNormalizeLongitude(t *testing.T) {
	cases := map[float64]float64{0: 0, 13.4: 13.4, -179.9: -179.9, 180: -180, 181: -179, -181: 179, 540: -180}
	for in, want := range cases {
		if got := normalizeLongitude(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("normalizeLongitude(%.1f) = %.6f, want %.1f", in, got, want)
		}
	}
}

// fakeTileClock is the pacing clock the tests run on: it only moves when the
// code under test waits, so a spacing assertion is exact and a paced fetch
// costs no real time.
type fakeTileClock struct {
	start time.Time
	now   time.Time
	slept []time.Duration
}

// stubTileClock installs a fake clock for the duration of the test.
func stubTileClock(t *testing.T) *fakeTileClock {
	t.Helper()

	start := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	clock := &fakeTileClock{start: start, now: start}
	origSleep, origNow := sleepFn, nowFn
	sleepFn = func(d time.Duration) {
		clock.slept = append(clock.slept, d)
		clock.now = clock.now.Add(d)
	}
	nowFn = func() time.Time { return clock.now }
	t.Cleanup(func() { sleepFn, nowFn = origSleep, origNow })
	return clock
}

func TestTankerLimiterBurst(t *testing.T) {
	clock := stubTileClock(t)

	lim := &tankerLimiter{delay: 30 * time.Second, burst: 3}
	for i := 0; i < 8; i++ {
		lim.wait()
	}
	// Three go out at once, the next three a window later, the last two the
	// window after that: eight requests cost two waits, not seven.
	if len(clock.slept) != 2 {
		t.Fatalf("8 requests at burst 3 slept %d times (%v), want 2", len(clock.slept), clock.slept)
	}
	for i, d := range clock.slept {
		if d != 30*time.Second {
			t.Fatalf("wait %d was %v, want 30s", i, d)
		}
	}
}

func TestTankerLimiterNeverWaitsWhenDisarmed(t *testing.T) {
	clock := stubTileClock(t)

	for _, lim := range []*tankerLimiter{
		nil,
		{delay: 0, burst: 3},
		{delay: 30 * time.Second, burst: 0},
	} {
		for i := 0; i < 10; i++ {
			lim.wait()
		}
	}
	if len(clock.slept) != 0 {
		t.Fatalf("a disarmed limiter slept %v", clock.slept)
	}
}

// tileStation renders one station the stub API can return, placed at a bearing
// and distance from a centre so the test controls where it lands.
func tileStation(id string, lat, lng, distKM, bearingDeg float64) string {
	stationLat, stationLng := destinationPoint(lat, lng, distKM, bearingDeg)
	return fmt.Sprintf(`{"id":%q,"name":%q,"brand":"Test","street":"Main","place":"Town",
		"lat":%f,"lng":%f,"dist":%f,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,
		"houseNumber":"1","postCode":12345}`, id, "Station "+id, stationLat, stationLng, distKM)
}

func tileListResponse(stations ...string) *http.Response {
	return jsonResponse(http.StatusOK, `{"ok":true,"stations":[`+strings.Join(stations, ",")+`]}`)
}

// stubTankerTiles answers every /list.php call from fn and records the query
// each one made.
func stubTankerTiles(t *testing.T, fn func(index int, lat, lng, rad float64) (*http.Response, error)) *[]string {
	t.Helper()

	var seen []string
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		if !strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php") {
			return nil, fmt.Errorf("unexpected request to %s", u)
		}
		lat, _ := strconv.ParseFloat(u.Query().Get("lat"), 64)
		lng, _ := strconv.ParseFloat(u.Query().Get("lng"), 64)
		rad, _ := strconv.ParseFloat(u.Query().Get("rad"), 64)
		index := len(seen)
		seen = append(seen, fmt.Sprintf("%s|%s|%s", u.Query().Get("lat"), u.Query().Get("lng"), u.Query().Get("rad")))
		return fn(index, lat, lng, rad)
	})
	t.Cleanup(restore)
	return &seen
}

func TestFetchTiledStationsDedupe(t *testing.T) {
	stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	// Every tile reports the same station in the middle plus one of its own.
	seen := stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		if rad != maxAPIRadiusKM {
			return nil, fmt.Errorf("tile %d asked for rad=%.2f, want %.2f", index, rad, maxAPIRadiusKM)
		}
		return tileListResponse(
			tileStation("shared", centre.Lat, centre.Lng, 1, 0),
			tileStation(fmt.Sprintf("own-%d", index), lat, lng, 2, 0),
		), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	stations, _, failed, err := fetchTiledStations(context.Background(), config{APIKey: "k"}, &tankerLimiter{}, centre, tiles, 50, "all", "dist")
	if err != nil {
		t.Fatalf("fetchTiledStations: %v", err)
	}
	if failed != 0 {
		t.Fatalf("tilesFailed = %d, want 0", failed)
	}
	if len(*seen) != len(tiles) {
		t.Fatalf("%d requests for %d tiles", len(*seen), len(tiles))
	}
	// Each tile is a distinct query.
	distinct := map[string]bool{}
	for _, q := range *seen {
		distinct[q] = true
	}
	if len(distinct) != len(tiles) {
		t.Fatalf("%d distinct queries for %d tiles: %v", len(distinct), len(tiles), *seen)
	}

	// The shared station is stored once, not once per tile.
	byID := map[string]int{}
	for _, s := range stations {
		byID[s.ID]++
	}
	if byID["shared"] != 1 {
		t.Fatalf("the shared station appears %d times", byID["shared"])
	}
	if len(stations) != len(tiles)+1 {
		t.Fatalf("%d stations, want %d (one per tile plus the shared one)", len(stations), len(tiles)+1)
	}
	// Dist is restated from the city centre, and the list comes back in that order.
	for i, s := range stations {
		want := haversineKM(centre.Lat, centre.Lng, s.Lat, s.Lng)
		if math.Abs(s.Dist-want) > 0.01 {
			t.Errorf("%s: Dist %.3f, want %.3f from the city centre", s.ID, s.Dist, want)
		}
		if i > 0 && stations[i-1].Dist > s.Dist {
			t.Errorf("station %d (%.3f) sorts after %d (%.3f)", i-1, stations[i-1].Dist, i, s.Dist)
		}
	}
}

func TestFetchTiledStationsFiltersOvershoot(t *testing.T) {
	stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	// The outermost tiles see past the radius that was asked for; only the
	// station inside it may be stored.
	stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		if index == 0 {
			return tileListResponse(tileStation("centre", centre.Lat, centre.Lng, 1, 0)), nil
		}
		return tileListResponse(
			tileStation(fmt.Sprintf("inside-%d", index), centre.Lat, centre.Lng, 49, float64(index)*40),
			tileStation(fmt.Sprintf("outside-%d", index), centre.Lat, centre.Lng, 54, float64(index)*40),
		), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	stations, _, _, err := fetchTiledStations(context.Background(), config{APIKey: "k"}, &tankerLimiter{}, centre, tiles, 50, "all", "dist")
	if err != nil {
		t.Fatalf("fetchTiledStations: %v", err)
	}
	for _, s := range stations {
		if strings.HasPrefix(s.ID, "outside-") {
			t.Errorf("%s is %.3f km out and should have been dropped", s.ID, s.Dist)
		}
		if d := haversineKM(centre.Lat, centre.Lng, s.Lat, s.Lng); d > 50 {
			t.Errorf("%s is %.3f km from the centre, past the 50 km asked for", s.ID, d)
		}
	}
	if len(stations) == 0 {
		t.Fatal("every station was dropped")
	}
}

func TestFetchTiledStationsPartialFailure(t *testing.T) {
	clock := stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	// One tile is permanently down; the rest answer. Its two attempts (the
	// first and the one retry) are both counted as requests.
	attempts := 0
	stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		attempts++
		if index >= 2 && index <= 3 {
			return jsonResponse(http.StatusBadGateway, `{"ok":false,"message":"upstream"}`), nil
		}
		return tileListResponse(tileStation(fmt.Sprintf("s-%d", index), lat, lng, 1, 0)), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	stations, _, failed, err := fetchTiledStations(context.Background(), config{APIKey: "k"}, &tankerLimiter{delay: 30 * time.Second, burst: 3}, centre, tiles, 50, "all", "dist")
	if err != nil {
		t.Fatalf("a failing tile must not fail the city: %v", err)
	}
	if failed != 1 {
		t.Fatalf("tilesFailed = %d, want 1", failed)
	}
	if len(stations) != len(tiles)-1 {
		t.Fatalf("%d stations, want %d (every tile but the failed one)", len(stations), len(tiles)-1)
	}
	// A 502 is worth exactly one retry, so the dead tile costs two requests.
	if attempts != len(tiles)+1 {
		t.Fatalf("%d requests for %d tiles, want %d (one retry)", attempts, len(tiles), len(tiles)+1)
	}
	if len(clock.slept) == 0 {
		t.Fatal("a tiled fetch with a delay set never waited")
	}
}

func TestFetchTiledStationsFirstTileFails(t *testing.T) {
	stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		if index == 0 {
			return jsonResponse(http.StatusUnauthorized, `{"ok":false,"message":"apikey nicht gefunden"}`), nil
		}
		return tileListResponse(tileStation("late", lat, lng, 1, 0)), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	_, _, _, err = fetchTiledStations(context.Background(), config{APIKey: "k"}, &tankerLimiter{}, centre, tiles, 50, "all", "dist")
	if err == nil {
		t.Fatal("a failing centre tile must fail the whole city")
	}
	// The message the sweep reports is the API's own, unchanged by the retry
	// classification wrapping it.
	if !strings.Contains(err.Error(), "tankerkönig request failed") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q, want the original request failure", err)
	}
}

// A 401 is permanent: retrying it costs a whole pacing window and cannot help.
func TestFetchTiledStationsDoesNotRetryPermanentFailures(t *testing.T) {
	stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	attempts := 0
	stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		attempts++
		if index == 1 {
			return jsonResponse(http.StatusUnauthorized, `{"ok":false,"message":"apikey nicht gefunden"}`), nil
		}
		return tileListResponse(tileStation(fmt.Sprintf("s-%d", index), lat, lng, 1, 0)), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	if _, _, failed, err := fetchTiledStations(context.Background(), config{APIKey: "k"}, &tankerLimiter{}, centre, tiles, 50, "all", "dist"); err != nil || failed != 1 {
		t.Fatalf("failed = %d, err = %v", failed, err)
	}
	if attempts != len(tiles) {
		t.Fatalf("%d requests for %d tiles: a permanent failure was retried", attempts, len(tiles))
	}
}

func TestSortStationsByPrice(t *testing.T) {
	price := func(v float64) *float64 { return &v }
	stations := []tankerStation{
		{ID: "c", E5: price(1.9), Dist: 1},
		{ID: "a", E5: nil, Dist: 2},
		{ID: "b", E5: price(1.5), Dist: 3},
	}
	sortStations(stations, "e5", "price")
	if got := []string{stations[0].ID, stations[1].ID, stations[2].ID}; got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("order = %v, want [b c a] (cheapest first, no price last)", got)
	}

	// "all" has no single price to sort on, so distance decides.
	stations = []tankerStation{{ID: "far", Dist: 9}, {ID: "near", Dist: 1}}
	sortStations(stations, "all", "price")
	if stations[0].ID != "near" {
		t.Fatalf("order = %s first, want the nearest", stations[0].ID)
	}
}

// A tile answer must survive the round trip through the merge unchanged, so the
// stub responses in these tests stay honest about the real payload shape.
func TestTileStationPayloadDecodes(t *testing.T) {
	var payload tankerListResponse
	body := `{"ok":true,"stations":[` + tileStation("x", 52.5, 13.4, 5, 90) + `]}`
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Stations) != 1 || payload.Stations[0].ID != "x" || !payload.Stations[0].IsOpen {
		t.Fatalf("decoded %+v", payload.Stations)
	}
}

// wait's return value is the contract the snapshot instant rests on, so it is
// asserted directly: the requests that fit in the current window go out now,
// and the one that does not goes out when the window rolls over.
func TestTankerLimiterWaitReportsSlot(t *testing.T) {
	clock := stubTileClock(t)

	lim := &tankerLimiter{delay: 30 * time.Second, burst: 3}
	for i := 0; i < 3; i++ {
		if got := lim.wait(); !got.Equal(clock.start) {
			t.Fatalf("request %d went out at %v, want %v", i+1, got, clock.start)
		}
	}
	if got, want := lim.wait(), clock.start.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("the request past the window went out at %v, want %v", got, want)
	}

	// A disarmed limiter reports the current instant and never sleeps, so an
	// untiled sweep is stamped exactly as it always was.
	for _, disarmed := range []*tankerLimiter{
		nil,
		{delay: 0, burst: 3},
		{delay: 30 * time.Second, burst: 0},
	} {
		before := len(clock.slept)
		if got := disarmed.wait(); !got.Equal(clock.now) {
			t.Fatalf("a disarmed limiter reported %v, want the current instant %v", got, clock.now)
		}
		if len(clock.slept) != before {
			t.Fatalf("a disarmed limiter slept")
		}
	}
}

// An attempt that failed observed nothing, so the instant the city is stamped
// with has to come from the attempt that actually answered.
func TestFetchTiledStationsObservedAtSkipsFailedAttempt(t *testing.T) {
	clock := stubTileClock(t)
	centre := cachedCity{QueryName: "Berlin", Name: "Berlin", Lat: 52.5, Lng: 13.4}

	calls := 0
	stubTankerTiles(t, func(index int, lat, lng, rad float64) (*http.Response, error) {
		calls++
		if calls == 1 { // the centre tile's first attempt, retryably
			return jsonResponse(http.StatusBadGateway, `{"ok":false,"message":"upstream"}`), nil
		}
		return tileListResponse(tileStation(fmt.Sprintf("s-%d", calls), lat, lng, 1, 0)), nil
	})

	tiles, err := planSearchTiles(centre.Lat, centre.Lng, 50)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	// burst 1 makes every request wait, so the retry lands a window after the
	// attempt it replaces and the two instants cannot be confused.
	lim := &tankerLimiter{delay: 30 * time.Second, burst: 1}
	_, observedAt, failed, err := fetchTiledStations(context.Background(), config{APIKey: "k"}, lim, centre, tiles, 50, "all", "dist")
	if err != nil {
		t.Fatalf("a retryable centre failure that then succeeds must not fail the city: %v", err)
	}
	if failed != 0 {
		t.Fatalf("tilesFailed = %d, want 0 — the retry answered", failed)
	}
	// The first attempt went out at start; the retry a window later.
	if want := clock.start.Add(30 * time.Second); !observedAt.Equal(want) {
		t.Fatalf("observedAt = %v, want %v (the retry, not the attempt that came back empty)", observedAt, want)
	}
}

func TestDefaultPaceFitsSweepBudget(t *testing.T) {
	tiles, err := planSearchTiles(52.2799, 8.6122, maxRequestRadiusKM)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	lim := &tankerLimiter{delay: defaultRequestDelay, burst: defaultRequestBurst}
	// The widest sweep the defaults have to carry: every tile of a 50 km target
	// plus the one retry a transient failure costs. Overrunning the budget does
	// not just finish late — flock drops the next run, so the sweep after it is
	// lost too.
	worst := lim.pace(len(tiles) + maxTileRetries)
	if worst > sweepBudget {
		t.Fatalf("a %d-tile sweep with one retry paces to %v, over the %v budget", len(tiles), worst, sweepBudget)
	}
	// And the pace is as slow as that budget allows, to within a window: a
	// default that leaves a whole further window unspent is being gentler on
	// the deadline than on the API key, which is the wrong way round.
	if worst+defaultRequestDelay <= sweepBudget {
		t.Fatalf("the defaults pace to %v and a whole further %v window still fits inside %v — widen --request-delay", worst, defaultRequestDelay, sweepBudget)
	}
}

func TestPaceMatchesTheLimiterItDescribes(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		burst int
	}{
		{35 * time.Second, 1},
		{30 * time.Second, 3},
		{90 * time.Second, 2},
		{0, 3},
		{30 * time.Second, 0},
	} {
		clock := stubTileClock(t)
		lim := &tankerLimiter{delay: tc.delay, burst: tc.burst}
		var last time.Time
		for i := 0; i < 9; i++ {
			last = lim.wait()
		}
		// pace is only trustworthy as a budget check while it agrees with what
		// the limiter actually does to the clock.
		if got, want := lim.pace(9), last.Sub(clock.start); got != want {
			t.Fatalf("delay %v burst %d: pace(9) = %v, but 9 requests took %v", tc.delay, tc.burst, got, want)
		}
	}
}

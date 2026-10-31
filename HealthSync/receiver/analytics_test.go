package main

import (
	"math"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestBucketStart(t *testing.T) {
	loc := mustLoc(t, "Europe/Brussels")
	ts := time.Date(2026, 10, 29, 23, 30, 0, 0, time.UTC) // Friday 00:30 local
	cases := map[string]string{
		"hour":  "2026-10-30 00:00",
		"day":   "2026-10-30 00:00",
		"week":  "2026-10-26 00:00",
		"month": "2026-10-01 00:00",
		"year":  "2026-01-01 00:00",
	}
	for b, want := range cases {
		if got := bucketStart(ts, b, loc).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%s: got %s, want %s", b, got, want)
		}
	}
}

func TestFoldHoursCumulativeTakesBiggestSource(t *testing.T) {
	loc := time.UTC
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := []hourRow{
		{Hour: day.Add(8 * time.Hour), Source: "Watch", Sum: 3000, N: 10, Min: 1, Max: 500},
		{Hour: day.Add(9 * time.Hour), Source: "Watch", Sum: 2000, N: 10, Min: 1, Max: 500},
		{Hour: day.Add(8 * time.Hour), Source: "iPhone", Sum: 4500, N: 10, Min: 1, Max: 500},
		{Hour: day.Add(30 * time.Hour), Source: "iPhone", Sum: 100, N: 1, Min: 100, Max: 100},
	}
	pts := foldHours(rows, "day", loc, true)
	if len(pts) != 2 || pts[0].Value != 5000 || pts[1].Value != 100 {
		t.Fatalf("got %+v", pts)
	}
}

func TestFoldHoursAverage(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := []hourRow{
		{Hour: day, Source: "Watch", Sum: 600, N: 10, Min: 50, Max: 70},
		{Hour: day.Add(time.Hour), Source: "Watch", Sum: 900, N: 10, Min: 80, Max: 120},
	}
	pts := foldHours(rows, "day", time.UTC, false)
	if len(pts) != 1 || pts[0].Value != 75 || *pts[0].Min != 50 || *pts[0].Max != 120 || pts[0].N != 20 {
		t.Fatalf("got %+v", pts)
	}
}

func TestSleepNightsPicksBestSource(t *testing.T) {
	loc := mustLoc(t, "Europe/Brussels")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	ss := []sleepSample{
		{at(29, 23, 0), at(30, 7, 0), "inBed", "iPhone"},
		{at(29, 23, 10), at(30, 1, 0), "core", "Watch"},
		{at(30, 1, 0), at(30, 2, 0), "deep", "Watch"},
		{at(30, 2, 0), at(30, 2, 15), "awake", "Watch"},
		{at(30, 2, 15), at(30, 6, 45), "rem", "Watch"},
		{at(29, 23, 30), at(30, 3, 0), "asleep", "Other app"},
		{at(30, 14, 0), at(30, 14, 30), "asleep", "Watch"}, // nap, same day
	}
	nights := sleepNights(ss, loc, true)
	if len(nights) != 1 {
		t.Fatalf("want 1 night, got %+v", nights)
	}
	n := nights[0]
	if n.Date != "2026-10-30" || n.Source != "Watch" {
		t.Fatalf("got %s from %s", n.Date, n.Source)
	}
	// 110 core + 60 deep + 270 rem + 30 nap
	if n.Asleep != 470 || n.Stages["awake"] != 15 || n.InBed != 480 {
		t.Fatalf("asleep %v, stages %v", n.Asleep, n.Stages)
	}
}

// line builds a straight route heading north at the given speed, one point per second.
func line(seconds int, mps float64) []routePoint {
	start := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	var pts []routePoint
	for i := 0; i <= seconds; i++ {
		lat := 50 + float64(i)*mps/111195 // metres per degree of latitude
		pts = append(pts, routePoint{T: start.Add(time.Duration(i) * time.Second), Lat: lat, Lon: 4, Alt: 10, HAccuracy: 5})
	}
	return cleanRoute(pts)
}

func TestSplitsAndBestEffort(t *testing.T) {
	pts := line(1500, 4) // 6 km at 4 m/s, 250 s per km
	total := pts[len(pts)-1].Dist
	if math.Abs(total-6000) > 5 {
		t.Fatalf("distance %v", total)
	}
	sp := splits(pts, 1000, nil)
	if len(sp) != 6 {
		t.Fatalf("%d splits: %+v", len(sp), sp)
	}
	for _, s := range sp {
		if math.Abs(s.Seconds-250) > 2 {
			t.Errorf("split %d took %v s", s.Index, s.Seconds)
		}
	}
	sec, ok := bestEffort(pts, 5000)
	if !ok || math.Abs(sec-1250) > 2 {
		t.Fatalf("5 km in %v (%v)", sec, ok)
	}
	if _, ok := bestEffort(pts, 10000); ok {
		t.Fatal("found a 10 km effort in a 6 km route")
	}
}

func TestCleanRouteDropsInaccurateFixes(t *testing.T) {
	pts := line(10, 4)
	pts = append(pts[:5:5], routePoint{T: pts[5].T, Lat: 51, Lon: 5, HAccuracy: 200})
	if got := cleanRoute(pts); len(got) != 5 {
		t.Fatalf("kept %d points", len(got))
	}
}

func TestElevationGainIgnoresNoise(t *testing.T) {
	alts := []float64{100, 101, 100, 101, 100, 105, 110, 109, 110, 104, 100}
	var pts []routePoint
	for _, a := range alts {
		pts = append(pts, routePoint{Alt: a})
	}
	gain, loss := elevationGain(pts, 3)
	if gain != 10 || loss != 10 {
		t.Fatalf("gain %v loss %v", gain, loss)
	}
}

func TestHRZones(t *testing.T) {
	start := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	hr := []hrSample{{start, 95}, {start.Add(60 * time.Second), 125}, {start.Add(120 * time.Second), 175}}
	z := hrZones(hr, 190, start.Add(180*time.Second))
	// 95/190 = 50 % zone 1, 125/190 = 66 % zone 2, 175/190 = 92 % zone 5
	if z[0].Seconds != 60 || z[1].Seconds != 60 || z[4].Seconds != 60 || z[2].Seconds != 0 {
		t.Fatalf("%+v", z)
	}
}

func TestSimplifyCapsPoints(t *testing.T) {
	pts := line(5000, 3)
	s := simplify(pts, 3, 500)
	if len(s) != 500 || s[0] != pts[0] || s[len(s)-1] != pts[len(pts)-1] {
		t.Fatalf("len %d", len(s))
	}
}

func TestUnitsAndQuantities(t *testing.T) {
	if toMetres(1, "mi") != 1609.344 || toMetres(2, "km") != 2000 || toKcal(4184, "J") != 1 {
		t.Fatal("unit conversion")
	}
	v, u, ok := parseQuantity("12345 cm")
	if !ok || v != 12345 || u != "cm" || toMetres(v, u) != 123.45 {
		t.Fatalf("%v %v %v", v, u, ok)
	}
	if ageOn("1980-11-15T00:00:00Z", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) != 45 {
		t.Fatal("age")
	}
}

package main

// Pure calculations behind the web app's API: bucketing, de-duplicating sources, sleep nights, route maths and heart
// rate zones. Nothing here touches the database, so it is all unit-tested.

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cumulativeTypes are added up per bucket (steps per day); every other quantity type is averaged (heart rate).
var cumulativeTypes = map[string]bool{
	"step_count": true, "distance_walking_running": true, "distance_cycling": true, "distance_swimming": true,
	"distance_wheelchair": true, "distance_downhill_snow_sports": true, "distance_cross_country_skiing": true,
	"distance_paddle_sports": true, "distance_rowing": true, "distance_skating_sports": true,
	"active_energy_burned": true, "basal_energy_burned": true, "flights_climbed": true, "push_count": true,
	"swimming_stroke_count": true, "apple_exercise_time": true, "apple_stand_time": true, "apple_move_time": true,
	"time_in_daylight": true, "number_of_times_fallen": true, "number_of_alcoholic_beverages": true,
	"nike_fuel": true, "insulin_delivery": true, "cross_country_skiing_stroke_count": true,
	"paddle_sports_stroke_count": true, "rowing_stroke_count": true,
}

func isCumulative(t string) bool {
	return cumulativeTypes[t] || strings.HasPrefix(t, "dietary_")
}

// hourRow is one (UTC hour, source) group from the database.
type hourRow struct {
	Hour   time.Time
	Source string
	Sum    float64
	N      int
	Min    float64
	Max    float64
}

// Point is one bucket of a series. For cumulative types Value is the total (from the source that recorded the most,
// so an iPhone and a Watch both counting steps aren't added together); otherwise it is the mean, with Min and Max.
type Point struct {
	T     int64    `json:"t"` // bucket start, Unix seconds
	Value float64  `json:"v"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
	N     int      `json:"n"`
}

// bucketStart is the local start of the hour, day, week (Monday), month or year containing t.
func bucketStart(t time.Time, bucket string, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	switch bucket {
	case "hour":
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	case "week":
		wd := (int(t.Weekday()) + 6) % 7 // Monday = 0
		return time.Date(y, m, d-wd, 0, 0, 0, 0, loc)
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	default:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
}

// autoBucket picks a bucket so a chart has between roughly 30 and 400 points.
func autoBucket(from, to time.Time) string {
	span := to.Sub(from)
	switch {
	case span <= 3*24*time.Hour:
		return "hour"
	case span <= 400*24*time.Hour:
		return "day"
	case span <= 4*365*24*time.Hour:
		return "week"
	default:
		return "month"
	}
}

// foldHours turns hourly per-source groups into local buckets.
func foldHours(rows []hourRow, bucket string, loc *time.Location, cumulative bool) []Point {
	type acc struct {
		sum      float64
		n        int
		min, max float64
		bySource map[string]float64
	}
	buckets := map[int64]*acc{}
	for _, r := range rows {
		k := bucketStart(r.Hour, bucket, loc).Unix()
		a := buckets[k]
		if a == nil {
			a = &acc{min: math.Inf(1), max: math.Inf(-1), bySource: map[string]float64{}}
			buckets[k] = a
		}
		a.sum += r.Sum
		a.n += r.N
		a.min = math.Min(a.min, r.Min)
		a.max = math.Max(a.max, r.Max)
		a.bySource[r.Source] += r.Sum
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		a := buckets[k]
		p := Point{T: k, N: a.n}
		if cumulative {
			for _, v := range a.bySource {
				p.Value = math.Max(p.Value, v)
			}
		} else if a.n > 0 {
			p.Value = a.sum / float64(a.n)
			mn, mx := a.min, a.max
			p.Min, p.Max = &mn, &mx
		}
		out = append(out, p)
	}
	return out
}

// ---- Sleep ----

type sleepSample struct {
	Start, End time.Time
	Stage      string // inBed, asleep, awake, core, deep, rem
	Source     string
}

// Night is one night of sleep, from the source that recorded the most sleep that night. Minutes per stage.
type Night struct {
	Date    string             `json:"date"` // the local date the night ends on
	Source  string             `json:"source"`
	Start   int64              `json:"start"` // first sleep, Unix seconds
	End     int64              `json:"end"`   // last wake
	Asleep  float64            `json:"asleep"`
	InBed   float64            `json:"inBed"`
	Stages  map[string]float64 `json:"stages"`
	Samples []sleepSegment     `json:"segments,omitempty"`
}

type sleepSegment struct {
	Start int64  `json:"s"`
	End   int64  `json:"e"`
	Stage string `json:"stage"`
}

// nightOf assigns a sleep sample to a night: the local date six hours after it starts, so sleep from 18:00 until the
// next 18:00 belongs to the day it ends on and an afternoon nap stays on its own day.
func nightOf(t time.Time, loc *time.Location) string {
	return t.In(loc).Add(6 * time.Hour).Format("2006-01-02")
}

func sleepNights(samples []sleepSample, loc *time.Location, withSegments bool) []Night {
	type key struct{ night, source string }
	groups := map[key][]sleepSample{}
	for _, s := range samples {
		k := key{nightOf(s.Start, loc), s.Source}
		groups[k] = append(groups[k], s)
	}
	// Time in bed often comes from another source (the iPhone) than the stages (the Watch); keep the longest.
	inBed := map[string]float64{}
	for k, ss := range groups {
		total := 0.0
		for _, s := range ss {
			if s.Stage == "inBed" {
				total += s.End.Sub(s.Start).Minutes()
			}
		}
		inBed[k.night] = math.Max(inBed[k.night], total)
	}
	best := map[string]Night{}
	for k, ss := range groups {
		n := Night{Date: k.night, Source: k.source, Stages: map[string]float64{}}
		var first, last time.Time
		for _, s := range ss {
			mins := s.End.Sub(s.Start).Minutes()
			if s.Stage == "inBed" {
				n.InBed += mins
				continue
			}
			n.Stages[s.Stage] += mins
			if s.Stage != "awake" {
				n.Asleep += mins
				if first.IsZero() || s.Start.Before(first) {
					first = s.Start
				}
				if s.End.After(last) {
					last = s.End
				}
			}
		}
		if !first.IsZero() {
			n.Start, n.End = first.Unix(), last.Unix()
		}
		if withSegments {
			sort.Slice(ss, func(i, j int) bool { return ss[i].Start.Before(ss[j].Start) })
			for _, s := range ss {
				n.Samples = append(n.Samples, sleepSegment{s.Start.Unix(), s.End.Unix(), s.Stage})
			}
		}
		if cur, ok := best[k.night]; !ok || n.Asleep > cur.Asleep || (n.Asleep == cur.Asleep && n.InBed > cur.InBed) {
			best[k.night] = n
		}
	}
	out := make([]Night, 0, len(best))
	for night, n := range best {
		n.InBed = inBed[night]
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// ---- Routes ----

type routePoint struct {
	T                time.Time
	Lat, Lon, Alt    float64
	Speed, HAccuracy float64
	Dist             float64 // metres from the start
}

const earthRadius = 6371008.8

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	r := math.Pi / 180
	dLat, dLon := (lat2-lat1)*r, (lon2-lon1)*r
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*r)*math.Cos(lat2*r)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadius * math.Asin(math.Min(1, math.Sqrt(a)))
}

// cleanRoute drops fixes with a poor horizontal accuracy and fills in the cumulative distance.
func cleanRoute(pts []routePoint) []routePoint {
	out := pts[:0:0]
	for _, p := range pts {
		if p.HAccuracy > 50 || (p.Lat == 0 && p.Lon == 0) {
			continue
		}
		if n := len(out); n > 0 {
			p.Dist = out[n-1].Dist + haversine(out[n-1].Lat, out[n-1].Lon, p.Lat, p.Lon)
		}
		out = append(out, p)
	}
	return out
}

// elevationGain sums climbs and descents, ignoring wiggles smaller than the threshold (GPS altitude is noisy).
func elevationGain(pts []routePoint, threshold float64) (gain, loss float64) {
	if len(pts) == 0 {
		return
	}
	ref := pts[0].Alt
	for _, p := range pts[1:] {
		d := p.Alt - ref
		if d >= threshold {
			gain += d
			ref = p.Alt
		} else if d <= -threshold {
			loss -= d
			ref = p.Alt
		}
	}
	return
}

// Split is one kilometre (or mile) of a workout.
type Split struct {
	Index    int      `json:"index"`
	Distance float64  `json:"distance"` // metres in this split (the last one is usually short)
	Seconds  float64  `json:"seconds"`
	Gain     float64  `json:"gain"`
	HR       *float64 `json:"hr,omitempty"`
}

// splits cuts the route every `every` metres. hrAt gives the average heart rate over a time range, or nil.
func splits(pts []routePoint, every float64, hrAt func(a, b time.Time) *float64) []Split {
	var out []Split
	if len(pts) < 2 || every <= 0 {
		return out
	}
	startIdx := 0
	next := every
	emit := func(endIdx int) {
		a, b := pts[startIdx], pts[endIdx]
		gain, _ := elevationGain(pts[startIdx:endIdx+1], 2)
		s := Split{Index: len(out) + 1, Distance: b.Dist - a.Dist, Seconds: b.T.Sub(a.T).Seconds(), Gain: gain}
		if hrAt != nil {
			s.HR = hrAt(a.T, b.T)
		}
		out = append(out, s)
		startIdx = endIdx
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].Dist >= next {
			emit(i)
			next += every
		}
	}
	if last := len(pts) - 1; startIdx < last && pts[last].Dist-pts[startIdx].Dist > 10 {
		emit(last)
	}
	return out
}

// bestEffort is the fastest time over `distance` metres anywhere in the route (two-pointer sweep).
func bestEffort(pts []routePoint, distance float64) (seconds float64, ok bool) {
	j := 0
	best := math.Inf(1)
	for i := 0; i < len(pts); i++ {
		for j < len(pts) && pts[j].Dist-pts[i].Dist < distance {
			j++
		}
		if j == len(pts) {
			break
		}
		// Interpolate the moment `distance` was reached between j-1 and j.
		d0, d1 := pts[j-1].Dist-pts[i].Dist, pts[j].Dist-pts[i].Dist
		t0, t1 := pts[j-1].T.Sub(pts[i].T).Seconds(), pts[j].T.Sub(pts[i].T).Seconds()
		t := t1
		if d1 > d0 {
			t = t0 + (t1-t0)*(distance-d0)/(d1-d0)
		}
		if t > 0 && t < best {
			best = t
		}
	}
	if math.IsInf(best, 1) {
		return 0, false
	}
	return best, true
}

// simplify keeps every point that moves at least minStep metres from the last kept one, and at most max points.
func simplify(pts []routePoint, minStep float64, max int) []routePoint {
	if len(pts) <= 2 {
		return pts
	}
	out := []routePoint{pts[0]}
	for _, p := range pts[1 : len(pts)-1] {
		if p.Dist-out[len(out)-1].Dist >= minStep {
			out = append(out, p)
		}
	}
	out = append(out, pts[len(pts)-1])
	if max > 0 && len(out) > max {
		step := float64(len(out)-1) / float64(max-1)
		thin := make([]routePoint, 0, max)
		for i := 0; i < max; i++ {
			thin = append(thin, out[int(math.Round(float64(i)*step))])
		}
		out = thin
	}
	return out
}

// ---- Heart rate ----

type hrSample struct {
	T   time.Time
	BPM float64
}

// hrBetween averages the heart rate samples in [a, b]. A plain mean is close enough at the Watch's workout rate
// (a sample every few seconds).
func hrBetween(hr []hrSample, a, b time.Time) *float64 {
	i := sort.Search(len(hr), func(i int) bool { return !hr[i].T.Before(a) })
	sum, n := 0.0, 0
	for ; i < len(hr) && !hr[i].T.After(b); i++ {
		sum += hr[i].BPM
		n++
	}
	if n == 0 {
		return nil
	}
	v := sum / float64(n)
	return &v
}

// Zone is the time spent between two fractions of maximum heart rate.
type Zone struct {
	Zone    int     `json:"zone"`
	Low     float64 `json:"low"` // bpm
	High    float64 `json:"high"`
	Seconds float64 `json:"seconds"`
}

// hrZones uses the classic five zones at 50/60/70/80/90 % of maximum. Each sample counts until the next one, capped at
// a minute so a gap in the data isn't credited to whatever came before it.
func hrZones(hr []hrSample, maxHR float64, end time.Time) []Zone {
	bounds := []float64{0.5, 0.6, 0.7, 0.8, 0.9, 1.0}
	zones := make([]Zone, 5)
	for i := range zones {
		zones[i] = Zone{Zone: i + 1, Low: math.Round(bounds[i] * maxHR), High: math.Round(bounds[i+1] * maxHR)}
	}
	for i, s := range hr {
		next := end
		if i+1 < len(hr) {
			next = hr[i+1].T
		}
		dt := math.Min(next.Sub(s.T).Seconds(), 60)
		if dt <= 0 {
			continue
		}
		f := s.BPM / maxHR
		z := -1
		for k := 4; k >= 0; k-- {
			if f >= bounds[k] {
				z = k
				break
			}
		}
		if z >= 0 {
			zones[z].Seconds += dt
		}
	}
	return zones
}

// ageOn returns whole years between a yyyy-mm-dd birth date and t, or 0.
func ageOn(dob string, t time.Time) int {
	b, err := time.Parse("2006-01-02", strings.TrimSpace(dob[:min(10, len(dob))]))
	if err != nil {
		return 0
	}
	age := t.Year() - b.Year()
	if t.YearDay() < b.YearDay() {
		age--
	}
	return age
}

// ---- Units ----

// toMetres converts a distance in the given HealthKit unit string to metres.
func toMetres(v float64, unit string) float64 {
	switch unit {
	case "km":
		return v * 1000
	case "mi":
		return v * 1609.344
	case "yd":
		return v * 0.9144
	case "ft":
		return v * 0.3048
	case "cm":
		return v / 100
	default:
		return v
	}
}

// toKcal converts energy to kilocalories.
func toKcal(v float64, unit string) float64 {
	switch unit {
	case "kJ":
		return v / 4.184
	case "J":
		return v / 4184
	case "cal":
		return v / 1000
	default: // kcal, Cal
		return v
	}
}

// parseQuantity reads metadata quantities as HealthKit prints them ("123.4 m", "25 degC") into a value and unit.
func parseQuantity(s string) (float64, string, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, "", false
	}
	if len(f) > 1 {
		return v, strings.Join(f[1:], " "), true
	}
	return v, "", true
}

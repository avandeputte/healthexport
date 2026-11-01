package main

// Read-only JSON API for the web app. Times in replies are Unix seconds; dates are local to the `tz` parameter (an
// IANA name the browser sends), falling back to the container's TZ.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errNotFound = errors.New("not found")

type WebAPI struct {
	db    *sql.DB
	cache *ttlCache
	// Best efforts per running workout, which never change unless the workout's route is resent.
	effortsMu   sync.Mutex
	effortCache map[string]workoutEfforts
	hourly      *rollupReady
	backfilling atomic.Bool
}

func NewWebAPI(db *sql.DB) *WebAPI {
	return &WebAPI{db: db, cache: newCache(2 * time.Minute), effortCache: map[string]workoutEfforts{}, hourly: &rollupReady{types: map[string]bool{}}}
}

// ---- cache ----

type cacheEntry struct {
	at  time.Time
	val any
}

type ttlCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]cacheEntry
}

func newCache(ttl time.Duration) *ttlCache { return &ttlCache{ttl: ttl, m: map[string]cacheEntry{}} }

func (c *ttlCache) get(key string, ttl time.Duration, f func() (any, error)) (any, error) {
	if ttl == 0 {
		ttl = c.ttl
	}
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && time.Since(e.at) < ttl {
		return e.val, nil
	}
	v, err := f()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.m) > 500 {
		c.m = map[string]cacheEntry{}
	}
	c.m[key] = cacheEntry{time.Now(), v}
	c.mu.Unlock()
	return v, nil
}

// Backfill fills sample_hourly for types that aren't complete yet, unless that is already running.
func (wa *WebAPI) Backfill() {
	if !wa.backfilling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer wa.backfilling.Store(false)
		backfillHourly(wa.db, wa.hourly, wa.cache.Clear)
	}()
}

// SyncFinished forgets cached replies so new data shows up right away, and rolls up any type seen for the first time.
func (wa *WebAPI) SyncFinished() {
	wa.Backfill()
	wa.cache.Clear()
	wa.effortsMu.Lock()
	wa.effortCache = map[string]workoutEfforts{}
	wa.effortsMu.Unlock()
}

// Clear drops everything, called when a sync finishes so new data shows up right away.
func (c *ttlCache) Clear() {
	c.mu.Lock()
	c.m = map[string]cacheEntry{}
	c.mu.Unlock()
}

func (wa *WebAPI) cached(w http.ResponseWriter, r *http.Request, ttl time.Duration, f func() (any, error)) {
	v, err := wa.cache.get(r.URL.Path+"?"+r.URL.RawQuery, ttl, f)
	if err != nil {
		var be badRequest
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if errors.As(err, &be) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type badRequest struct{ error }

// ---- parameters ----

func location(r *http.Request) *time.Location {
	if tz := r.URL.Query().Get("tz"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// timeRange reads from/to as local dates (to inclusive) or Unix seconds. The default is the last `days` days.
func timeRange(r *http.Request, loc *time.Location, days int) (time.Time, time.Time, error) {
	q := r.URL.Query()
	parse := func(s string, end bool) (time.Time, error) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0), nil
		}
		d, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			return time.Time{}, badRequest{fmt.Errorf("bad date %q, want yyyy-mm-dd", s)}
		}
		if end {
			d = d.AddDate(0, 0, 1)
		}
		return d, nil
	}
	now := time.Now().In(loc)
	y, m, d := now.Date()
	to := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -days)
	var err error
	if s := q.Get("from"); s != "" {
		if from, err = parse(s, false); err != nil {
			return from, to, err
		}
	}
	if s := q.Get("to"); s != "" {
		if to, err = parse(s, true); err != nil {
			return from, to, err
		}
	}
	if !to.After(from) {
		return from, to, badRequest{errors.New("to must be after from")}
	}
	return from.UTC(), to.UTC(), nil
}

// ---- status and types ----

func (wa *WebAPI) status(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 30*time.Second, func() (any, error) {
		v, err := schemaVersion(wa.db)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"schemaVersion": v, "tiles": map[string]string{
			"url":         env("MAP_TILE_URL", "https://tile.openstreetmap.org/{z}/{x}/{y}.png"),
			"attribution": env("MAP_TILE_ATTRIBUTION", `&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors`),
		}}
		var started, finished sql.NullTime
		var device sql.NullString
		var items sql.NullInt64
		err = wa.db.QueryRow("SELECT started_at, finished_at, device, items FROM sync_log ORDER BY id DESC LIMIT 1").
			Scan(&started, &finished, &device, &items)
		if err == nil {
			out["lastSync"] = map[string]any{"started": unix(started), "finished": unix(finished), "device": device.String, "items": items.Int64}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var latest sql.NullTime
		if err := wa.db.QueryRow("SELECT MAX(inserted_at) FROM samples").Scan(&latest); err == nil {
			out["latestArrival"] = unix(latest)
		}
		return out, nil
	})
}

func unix(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Unix()
}

type typeInfo struct {
	Type       string `json:"type"`
	Kind       string `json:"kind"`
	Unit       string `json:"unit"`
	Count      int64  `json:"count"`
	First      int64  `json:"first"`
	Last       int64  `json:"last"`
	Cumulative bool   `json:"cumulative"`
}

func (wa *WebAPI) typeList() ([]typeInfo, error) {
	v, err := wa.cache.get("types", 10*time.Minute, func() (any, error) {
		// Counts come from sample_hourly for the types it has complete, and from the samples index for the rest;
		// kind and unit come from one row per type.
		var out []typeInfo
		names, err := queryStrings(wa.db, "SELECT DISTINCT type FROM samples")
		if err != nil {
			return nil, err
		}
		rolled := map[string]typeInfo{}
		rows, err := wa.db.Query("SELECT type, SUM(n), MIN(hour), MAX(hour) FROM sample_hourly GROUP BY type")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t typeInfo
			var first, last time.Time
			if err := rows.Scan(&t.Type, &t.Count, &first, &last); err != nil {
				rows.Close()
				return nil, err
			}
			t.First, t.Last = first.Unix(), last.Unix()
			rolled[t.Type] = t
		}
		rows.Close()
		for _, name := range names {
			if t, ok := rolled[name]; ok && wa.hourly.has(name) {
				out = append(out, t)
				continue
			}
			t := typeInfo{Type: name}
			var first, last time.Time
			if err := wa.db.QueryRow("SELECT COUNT(*), MIN(start_date), MAX(start_date) FROM samples WHERE type = ?", name).
				Scan(&t.Count, &first, &last); err != nil {
				return nil, err
			}
			t.First, t.Last = first.Unix(), last.Unix()
			out = append(out, t)
		}
		for i := range out {
			var unit sql.NullString
			if err := wa.db.QueryRow("SELECT kind, unit FROM samples WHERE type = ? ORDER BY start_date DESC LIMIT 1", out[i].Type).
				Scan(&out[i].Kind, &unit); err != nil {
				return nil, err
			}
			out[i].Unit = unit.String
			out[i].Cumulative = out[i].Kind == "category" || isCumulative(out[i].Type)
			if out[i].Kind == "category" {
				out[i].Unit = "min"
			}
			if out[i].Type == "sleep_analysis" { // hours asleep per night, see sleepSeries
				out[i].Unit, out[i].Cumulative = "h", false
			}
		}
		rings, err := wa.ringTypes()
		if err != nil {
			return nil, err
		}
		out = append(out, rings...)
		sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]typeInfo), nil
}

func (wa *WebAPI) types(w http.ResponseWriter, r *http.Request) {
	t, err := wa.typeList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (wa *WebAPI) typeInfo(name string) (typeInfo, bool, error) {
	all, err := wa.typeList()
	if err != nil {
		return typeInfo{}, false, err
	}
	for _, t := range all {
		if t.Type == name {
			return t, true, nil
		}
	}
	return typeInfo{Type: name}, false, nil
}

// ---- series ----

type seriesReply struct {
	Type       string  `json:"type"`
	Unit       string  `json:"unit"`
	Bucket     string  `json:"bucket"`
	Cumulative bool    `json:"cumulative"`
	Points     []Point `json:"points"`
}

func (wa *WebAPI) series(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		loc := location(r)
		from, to, err := timeRange(r, loc, 30)
		if err != nil {
			return nil, err
		}
		name := r.URL.Query().Get("type")
		bucket := r.URL.Query().Get("bucket")
		if bucket == "" || bucket == "auto" {
			bucket = autoBucket(from, to)
		}
		return wa.seriesFor(name, from, to, bucket, loc)
	})
}

func (wa *WebAPI) seriesFor(name string, from, to time.Time, bucket string, loc *time.Location) (*seriesReply, error) {
	info, ok, err := wa.typeInfo(name)
	if err != nil {
		return nil, err
	}
	reply := &seriesReply{Type: name, Unit: info.Unit, Bucket: bucket, Cumulative: info.Cumulative, Points: []Point{}}
	if !ok {
		return reply, nil
	}
	if name == "sleep_analysis" {
		return wa.sleepSeries(from, to, bucket, loc)
	}
	if info.Kind == "ring" {
		return wa.ringSeries(info, from, to, bucket, loc)
	}
	switch bucket {
	case "raw":
		if info.Kind == "category" {
			return nil, badRequest{errors.New("raw is only for quantity types")}
		}
		rows, err := wa.db.Query(`SELECT start_date, value FROM samples WHERE type = ? AND start_date >= ? AND start_date < ?
			AND value IS NOT NULL ORDER BY start_date LIMIT 20000`, name, from, to)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var t time.Time
			var v float64
			if err := rows.Scan(&t, &v); err != nil {
				return nil, err
			}
			reply.Points = append(reply.Points, Point{T: t.Unix(), Value: v, N: 1})
		}
		return reply, rows.Err()
	case "hour", "day", "week", "month", "year":
	default:
		return nil, badRequest{fmt.Errorf("bucket must be raw, hour, day, week, month or year")}
	}
	val := "value"
	cond := "AND value IS NOT NULL"
	if info.Kind == "category" {
		val, cond = "TIMESTAMPDIFF(SECOND, start_date, end_date) / 60", ""
	}
	var hours []hourRow
	if wa.hourly.has(name) {
		hours, err = wa.hourlyRollup(name, from, to)
	} else {
		hours, err = wa.hourlySamples(name, val, cond, from, to)
	}
	if err != nil {
		return nil, err
	}
	reply.Points = foldHours(hours, bucket, loc, info.Cumulative)
	return reply, nil
}

// sleepSeries charts hours asleep per night (from the same night logic as the sleep page), averaged per bucket.
func (wa *WebAPI) sleepSeries(from, to time.Time, bucket string, loc *time.Location) (*seriesReply, error) {
	if bucket == "raw" || bucket == "hour" {
		bucket = "day"
	}
	reply := &seriesReply{Type: "sleep_analysis", Unit: "h", Bucket: bucket, Points: []Point{}}
	ss, err := wa.sleepSamples(from.Add(-6*time.Hour), to.Add(-6*time.Hour))
	if err != nil {
		return nil, err
	}
	var rows []hourRow
	for _, n := range sleepNights(ss, loc, false) {
		d, err := time.ParseInLocation("2006-01-02", n.Date, loc)
		if err != nil || n.Asleep == 0 {
			continue
		}
		h := n.Asleep / 60
		rows = append(rows, hourRow{Hour: d, Sum: h, N: 1, Min: h, Max: h})
	}
	reply.Points = foldHours(rows, bucket, loc, false)
	return reply, nil
}

// hourlyRollup reads the same groups from sample_hourly.
func (wa *WebAPI) hourlyRollup(name string, from, to time.Time) ([]hourRow, error) {
	rows, err := wa.db.Query(`SELECT hour, source_name, COALESCE(sum, 0), n, COALESCE(min, 0), COALESCE(max, 0) FROM sample_hourly
		WHERE type = ? AND hour >= ? AND hour < ?`, name, from.Truncate(time.Hour), to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hourRow
	for rows.Next() {
		var r hourRow
		if err := rows.Scan(&r.Hour, &r.Source, &r.Sum, &r.N, &r.Min, &r.Max); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// hourly groups a type's samples by UTC hour and source. Hours are folded into local buckets in Go, which keeps time
// zones right without MariaDB's time zone tables.
func (wa *WebAPI) hourlySamples(name, val, cond string, from, to time.Time) ([]hourRow, error) {
	q := fmt.Sprintf(`SELECT DATE_FORMAT(start_date, '%%Y-%%m-%%d %%H:00:00') AS h, COALESCE(source_name, ''),
		SUM(%[1]s), COUNT(*), MIN(%[1]s), MAX(%[1]s)
		FROM samples WHERE type = ? AND start_date >= ? AND start_date < ? %[2]s
		GROUP BY h, source_name`, val, cond)
	rows, err := wa.db.Query(q, name, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hourRow
	for rows.Next() {
		var h string
		var row hourRow
		if err := rows.Scan(&h, &row.Source, &row.Sum, &row.N, &row.Min, &row.Max); err != nil {
			return nil, err
		}
		if row.Hour, err = time.Parse("2006-01-02 15:04:05", h); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---- workouts ----

type workoutRow struct {
	UUID         string   `json:"uuid"`
	ActivityType string   `json:"activityType"`
	Start        int64    `json:"start"`
	End          int64    `json:"end"`
	Duration     float64  `json:"duration"` // seconds
	Source       string   `json:"source"`
	Distance     *float64 `json:"distance,omitempty"` // metres
	Energy       *float64 `json:"energy,omitempty"`   // kcal
	HRAvg        *float64 `json:"hrAvg,omitempty"`
	HRMax        *float64 `json:"hrMax,omitempty"`
	Elevation    *float64 `json:"elevation,omitempty"` // metres climbed, from the workout's metadata
	Indoor       bool     `json:"indoor,omitempty"`
	HasRoute     bool     `json:"hasRoute"`
}

func (wa *WebAPI) workoutList(from, to time.Time) ([]workoutRow, error) {
	rows, err := wa.db.Query(`SELECT w.uuid, COALESCE(w.activity_type, ''), w.start_date, w.end_date, COALESCE(w.duration_s, 0),
		COALESCE(w.source_name, ''), COALESCE(w.metadata_json, '{}'),
		EXISTS(SELECT 1 FROM workout_route_points p WHERE p.workout_uuid = w.uuid LIMIT 1)
		FROM workouts w WHERE w.start_date >= ? AND w.start_date < ? ORDER BY w.start_date DESC`, from, to)
	if err != nil {
		return nil, err
	}
	var list []workoutRow
	index := map[string]int{}
	for rows.Next() {
		var x workoutRow
		var s, e time.Time
		var meta string
		if err := rows.Scan(&x.UUID, &x.ActivityType, &s, &e, &x.Duration, &x.Source, &meta, &x.HasRoute); err != nil {
			rows.Close()
			return nil, err
		}
		x.Start, x.End = s.Unix(), e.Unix()
		applyWorkoutMeta(&x, meta)
		index[x.UUID] = len(list)
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return []workoutRow{}, nil
	}
	st, err := wa.db.Query(`SELECT s.workout_uuid, s.type, COALESCE(s.unit, ''), s.average, s.maximum, s.sum
		FROM workout_statistics s JOIN workouts w ON w.uuid = s.workout_uuid
		WHERE w.start_date >= ? AND w.start_date < ?
		AND (s.type LIKE 'distance%' OR s.type = 'active_energy_burned' OR s.type = 'heart_rate')`, from, to)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	for st.Next() {
		var id, typ, unit string
		var avg, max, sum sql.NullFloat64
		if err := st.Scan(&id, &typ, &unit, &avg, &max, &sum); err != nil {
			return nil, err
		}
		i, ok := index[id]
		if !ok {
			continue
		}
		x := &list[i]
		switch {
		case typ == "heart_rate":
			x.HRAvg, x.HRMax = ptr(avg), ptr(max)
		case typ == "active_energy_burned" && sum.Valid:
			v := toKcal(sum.Float64, unit)
			x.Energy = &v
		case strings.HasPrefix(typ, "distance") && sum.Valid:
			v := toMetres(sum.Float64, unit)
			if x.Distance == nil || v > *x.Distance {
				x.Distance = &v
			}
		}
	}
	return list, st.Err()
}

func applyWorkoutMeta(x *workoutRow, meta string) {
	var m map[string]string
	if json.Unmarshal([]byte(meta), &m) != nil {
		return
	}
	if v, u, ok := parseQuantity(m["HKElevationAscended"]); ok {
		e := toMetres(v, u)
		x.Elevation = &e
	}
	x.Indoor = m["HKIndoorWorkout"] == "1" || m["HKIndoorWorkout"] == "true"
}

func ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func (wa *WebAPI) workouts(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		from, to, err := timeRange(r, location(r), 365*30)
		if err != nil {
			return nil, err
		}
		list, err := wa.workoutList(from, to)
		if err != nil {
			return nil, err
		}
		if t := r.URL.Query().Get("type"); t != "" {
			out := list[:0:0]
			for _, x := range list {
				if x.ActivityType == t {
					out = append(out, x)
				}
			}
			list = out
		}
		return list, nil
	})
}

type workoutDetail struct {
	workoutRow
	Metadata   map[string]string       `json:"metadata"`
	Statistics []map[string]any        `json:"statistics"`
	Events     []map[string]any        `json:"events"`
	Activities []map[string]any        `json:"activities"`
	Route      [][]float64             `json:"route"` // [t, lat, lon, alt, dist, speed, hr]
	RouteGain  float64                 `json:"routeGain"`
	Splits     map[string][]Split      `json:"splits"`
	Efforts    []effort                `json:"efforts"`
	Series     map[string][][2]float64 `json:"series"` // type -> [t, value]
	Units      map[string]string       `json:"units"`
	MaxHR      float64                 `json:"maxHr"`
	MaxHRFrom  string                  `json:"maxHrFrom"`
	Zones      []Zone                  `json:"zones"`
}

type effort struct {
	Name     string  `json:"name"`
	Distance float64 `json:"distance"`
	Seconds  float64 `json:"seconds"`
}

var effortDistances = []struct {
	name string
	m    float64
}{{"400 m", 400}, {"1 km", 1000}, {"1 mile", 1609.344}, {"5 km", 5000}, {"10 km", 10000}, {"Half marathon", 21097.5}, {"Marathon", 42195}}

// Quantity types worth plotting inside a workout.
var workoutSeriesTypes = []string{"heart_rate", "running_power", "running_speed", "running_stride_length",
	"running_vertical_oscillation", "running_ground_contact_time", "cycling_power", "cycling_cadence", "cycling_speed",
	"physical_effort"}

func (wa *WebAPI) workout(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		id := r.PathValue("uuid")
		var d workoutDetail
		var s, e time.Time
		var meta string
		err := wa.db.QueryRow(`SELECT uuid, COALESCE(activity_type, ''), start_date, end_date, COALESCE(duration_s, 0),
			COALESCE(source_name, ''), COALESCE(metadata_json, '{}') FROM workouts WHERE uuid = ?`, id).
			Scan(&d.UUID, &d.ActivityType, &s, &e, &d.Duration, &d.Source, &meta)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		d.Start, d.End = s.Unix(), e.Unix()
		applyWorkoutMeta(&d.workoutRow, meta)
		json.Unmarshal([]byte(meta), &d.Metadata)

		if d.Statistics, err = queryMaps(wa.db, `SELECT type, unit, average, minimum, maximum, sum, most_recent
			FROM workout_statistics WHERE workout_uuid = ? ORDER BY type`, id); err != nil {
			return nil, err
		}
		for _, st := range d.Statistics {
			typ, _ := st["type"].(string)
			unit, _ := st["unit"].(string)
			switch {
			case typ == "heart_rate":
				d.HRAvg, d.HRMax = floatPtr(st["average"]), floatPtr(st["maximum"])
			case typ == "active_energy_burned":
				if v := floatPtr(st["sum"]); v != nil {
					k := toKcal(*v, unit)
					d.Energy = &k
				}
			case strings.HasPrefix(typ, "distance"):
				if v := floatPtr(st["sum"]); v != nil {
					m := toMetres(*v, unit)
					d.Distance = &m
				}
			}
		}
		if d.Events, err = queryMaps(wa.db, `SELECT type, UNIX_TIMESTAMP(start_date) AS start, UNIX_TIMESTAMP(end_date) AS end
			FROM workout_events WHERE workout_uuid = ? ORDER BY start_date`, id); err != nil {
			return nil, err
		}
		if d.Activities, err = queryMaps(wa.db, `SELECT activity_type, UNIX_TIMESTAMP(start_date) AS start, UNIX_TIMESTAMP(end_date) AS end
			FROM workout_activities WHERE workout_uuid = ? ORDER BY start_date`, id); err != nil {
			return nil, err
		}

		// Series recorded during the workout.
		d.Series, d.Units = map[string][][2]float64{}, map[string]string{}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(workoutSeriesTypes)), ",")
		args := []any{s, e}
		for _, t := range workoutSeriesTypes {
			args = append(args, t)
		}
		rows, err := wa.db.Query(`SELECT type, start_date, value, COALESCE(unit, '') FROM samples
			WHERE start_date >= ? AND start_date <= ? AND type IN (`+ph+`) AND value IS NOT NULL ORDER BY start_date`, args...)
		if err != nil {
			return nil, err
		}
		var hr []hrSample
		for rows.Next() {
			var typ, unit string
			var t time.Time
			var v float64
			if err := rows.Scan(&typ, &t, &v, &unit); err != nil {
				rows.Close()
				return nil, err
			}
			d.Series[typ] = append(d.Series[typ], [2]float64{float64(t.Unix()), v})
			d.Units[typ] = unit
			if typ == "heart_rate" {
				hr = append(hr, hrSample{t, v})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		maxHR, from, err := wa.maxHR(r, e)
		if err != nil {
			return nil, err
		}
		d.MaxHR, d.MaxHRFrom = maxHR, from
		if len(hr) > 0 && maxHR > 0 {
			d.Zones = hrZones(hr, maxHR, e)
		}

		pts, err := wa.routePoints(id)
		if err != nil {
			return nil, err
		}
		d.Route, d.Splits, d.Efforts = [][]float64{}, map[string][]Split{}, []effort{}
		if len(pts) > 1 {
			if d.Distance == nil {
				m := pts[len(pts)-1].Dist
				d.Distance = &m
			}
			d.RouteGain, _ = elevationGain(pts, 3)
			hrAt := func(a, b time.Time) *float64 { return hrBetween(hr, a, b) }
			d.Splits["km"] = splits(pts, 1000, hrAt)
			d.Splits["mi"] = splits(pts, 1609.344, hrAt)
			for _, ed := range effortDistances {
				if sec, ok := bestEffort(pts, ed.m); ok {
					d.Efforts = append(d.Efforts, effort{ed.name, ed.m, math.Round(sec)})
				}
			}
			for _, p := range simplify(pts, 3, 3000) {
				h := math.NaN()
				if v := hrBetween(hr, p.T.Add(-5*time.Second), p.T.Add(5*time.Second)); v != nil {
					h = *v
				}
				d.Route = append(d.Route, []float64{float64(p.T.Unix()), round(p.Lat, 6), round(p.Lon, 6), round(p.Alt, 1),
					round(p.Dist, 1), round(p.Speed, 2), round(h, 0)})
			}
		}
		return d, nil
	})
}

func round(v float64, places int) float64 {
	if math.IsNaN(v) {
		return -1
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func floatPtr(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

func (wa *WebAPI) routePoints(id string) ([]routePoint, error) {
	rows, err := wa.db.Query(`SELECT time, latitude, longitude, COALESCE(altitude, 0), COALESCE(speed, -1),
		COALESCE(horizontal_accuracy, 0) FROM workout_route_points WHERE workout_uuid = ? ORDER BY time`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []routePoint
	for rows.Next() {
		var p routePoint
		if err := rows.Scan(&p.T, &p.Lat, &p.Lon, &p.Alt, &p.Speed, &p.HAccuracy); err != nil {
			return nil, err
		}
		pts = append(pts, p)
	}
	return cleanRoute(pts), rows.Err()
}

// maxHR is the `maxHr` parameter if given, else 220 minus age from the date of birth, else the highest heart rate
// recorded in the last year.
func (wa *WebAPI) maxHR(r *http.Request, at time.Time) (float64, string, error) {
	if v, err := strconv.ParseFloat(r.URL.Query().Get("maxHr"), 64); err == nil && v > 100 && v < 250 {
		return v, "setting", nil
	}
	var dob sql.NullString
	err := wa.db.QueryRow("SELECT value FROM characteristics WHERE `key` = 'dateOfBirth'").Scan(&dob)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	if age := ageOn(dob.String, at); age > 5 && age < 110 {
		return float64(220 - age), "age", nil
	}
	var mx sql.NullFloat64
	if err := wa.db.QueryRow("SELECT MAX(value) FROM samples WHERE type = 'heart_rate' AND start_date >= ?", at.AddDate(-1, 0, 0)).Scan(&mx); err != nil {
		return 0, "", err
	}
	if mx.Valid && mx.Float64 > 100 {
		return mx.Float64, "observed", nil
	}
	return 0, "", nil
}

func queryStrings(db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryMaps returns rows as column -> value maps ([]byte turned into strings or numbers).
func queryMaps(db *sql.DB, q string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				s := string(v)
				if f, err := strconv.ParseFloat(s, 64); err == nil && c != "type" && c != "unit" && c != "activity_type" {
					m[c] = f
				} else {
					m[c] = s
				}
			case time.Time:
				m[c] = v.Unix()
			default:
				m[c] = v
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- all routes ----

type routeLine struct {
	UUID         string       `json:"uuid"`
	ActivityType string       `json:"activityType"`
	Start        int64        `json:"start"`
	Distance     float64      `json:"distance"`
	Points       [][2]float64 `json:"points"`
}

func (wa *WebAPI) routes(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 10*time.Minute, func() (any, error) {
		from, to, err := timeRange(r, location(r), 365)
		if err != nil {
			return nil, err
		}
		typ := r.URL.Query().Get("type")
		rows, err := wa.db.Query(`SELECT p.workout_uuid, COALESCE(w.activity_type, ''), w.start_date, p.time, p.latitude, p.longitude,
			COALESCE(p.horizontal_accuracy, 0)
			FROM workouts w JOIN workout_route_points p ON p.workout_uuid = w.uuid
			WHERE w.start_date >= ? AND w.start_date < ? AND (? = '' OR w.activity_type = ?)
			ORDER BY p.workout_uuid, p.time`, from, to, typ, typ)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []routeLine{}
		var cur *routeLine
		var pts []routePoint
		flush := func() {
			if cur == nil {
				return
			}
			pts = cleanRoute(pts)
			if len(pts) > 1 {
				cur.Distance = math.Round(pts[len(pts)-1].Dist)
				for _, p := range simplify(pts, 25, 600) {
					cur.Points = append(cur.Points, [2]float64{round(p.Lat, 5), round(p.Lon, 5)})
				}
				out = append(out, *cur)
			}
			pts = pts[:0]
		}
		for rows.Next() {
			var id, at string
			var start time.Time
			var p routePoint
			if err := rows.Scan(&id, &at, &start, &p.T, &p.Lat, &p.Lon, &p.HAccuracy); err != nil {
				return nil, err
			}
			if cur == nil || cur.UUID != id {
				flush()
				cur = &routeLine{UUID: id, ActivityType: at, Start: start.Unix()}
			}
			pts = append(pts, p)
		}
		flush()
		sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
		return out, rows.Err()
	})
}

// ---- sleep ----

func (wa *WebAPI) sleepSamples(from, to time.Time) ([]sleepSample, error) {
	rows, err := wa.db.Query(`SELECT start_date, end_date, COALESCE(text, ''), COALESCE(source_name, '') FROM samples
		WHERE type = 'sleep_analysis' AND start_date >= ? AND start_date < ? ORDER BY start_date`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sleepSample
	for rows.Next() {
		var s sleepSample
		if err := rows.Scan(&s.Start, &s.End, &s.Stage, &s.Source); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GET /api/sleep?from&to returns nights; with date=yyyy-mm-dd it returns that one night with its segments.
func (wa *WebAPI) sleep(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		loc := location(r)
		if date := r.URL.Query().Get("date"); date != "" {
			day, err := time.ParseInLocation("2006-01-02", date, loc)
			if err != nil {
				return nil, badRequest{err}
			}
			ss, err := wa.sleepSamples(day.Add(-6*time.Hour).UTC(), day.Add(18*time.Hour).UTC())
			if err != nil {
				return nil, err
			}
			for _, n := range sleepNights(ss, loc, true) {
				if n.Date == date {
					return n, nil
				}
			}
			return nil, errNotFound
		}
		from, to, err := timeRange(r, loc, 30)
		if err != nil {
			return nil, err
		}
		// Samples from six hours before the first day belong to its night.
		ss, err := wa.sleepSamples(from.Add(-6*time.Hour), to.Add(-6*time.Hour))
		if err != nil {
			return nil, err
		}
		return sleepNights(ss, loc, false), nil
	})
}

// ---- activity rings ----

func (wa *WebAPI) activity(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		loc := location(r)
		from, to, err := timeRange(r, loc, 30)
		if err != nil {
			return nil, err
		}
		return queryMaps(wa.db, `SELECT DATE_FORMAT(date, '%Y-%m-%d') AS date, active_energy_kcal AS move, active_energy_goal_kcal AS moveGoal,
			exercise_min AS exercise, exercise_goal_min AS exerciseGoal, stand_hours AS stand, stand_hours_goal AS standGoal
			FROM activity_summaries WHERE date >= ? AND date < ? ORDER BY date`,
			from.In(loc).Format("2006-01-02"), to.In(loc).Format("2006-01-02"))
	})
}

// ---- profile ----

func (wa *WebAPI) profile(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		rows, err := wa.db.Query("SELECT `key`, value FROM characteristics")
		if err != nil {
			return nil, err
		}
		chars := map[string]string{}
		for rows.Next() {
			var k string
			var v sql.NullString
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return nil, err
			}
			chars[k] = v.String
		}
		rows.Close()
		out := map[string]any{"characteristics": chars}
		now := time.Now()
		if age := ageOn(chars["dateOfBirth"], now); age > 0 {
			out["age"] = age
		}
		mx, from, err := wa.maxHR(r, now)
		if err != nil {
			return nil, err
		}
		out["maxHr"], out["maxHrFrom"] = mx, from
		var rhr sql.NullFloat64
		if err := wa.db.QueryRow(`SELECT AVG(value) FROM samples WHERE type = 'resting_heart_rate' AND start_date >= ?`,
			now.AddDate(0, 0, -30)).Scan(&rhr); err != nil {
			return nil, err
		}
		if rhr.Valid {
			out["restingHr"] = math.Round(rhr.Float64)
		}
		return out, nil
	})
}

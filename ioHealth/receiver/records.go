package main

import (
	"math"
	"net/http"
	"sort"
	"time"
)

// Record is one personal best.
type Record struct {
	Group string  `json:"group"` // "workout", "day", "health", "rings"
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	When  int64   `json:"when"` // Unix seconds (start of the day or workout)
	UUID  string  `json:"uuid,omitempty"`
	Type  string  `json:"type,omitempty"` // activity or sample type
}

type activityTotals struct {
	ActivityType string  `json:"activityType"`
	Count        int     `json:"count"`
	Duration     float64 `json:"duration"`
	Distance     float64 `json:"distance"`
	Energy       float64 `json:"energy"`
	First        int64   `json:"first"`
}

// GET /api/records: bests per activity type, best days, best health readings and ring streaks.
func (wa *WebAPI) records(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 10*time.Minute, func() (any, error) {
		loc := location(r)
		all, err := wa.workoutList(time.Unix(0, 0), time.Now().Add(24*time.Hour))
		if err != nil {
			return nil, err
		}
		var recs []Record
		totals := map[string]*activityTotals{}
		best := map[string]Record{}
		consider := func(key string, rec Record, higher bool) {
			cur, ok := best[key]
			if !ok || (higher && rec.Value > cur.Value) || (!higher && rec.Value < cur.Value) {
				best[key] = rec
			}
		}
		for _, x := range all {
			t := totals[x.ActivityType]
			if t == nil {
				t = &activityTotals{ActivityType: x.ActivityType, First: x.Start}
				totals[x.ActivityType] = t
			}
			t.Count++
			t.Duration += x.Duration
			t.First = min(t.First, x.Start)
			base := Record{Group: "workout", When: x.Start, UUID: x.UUID, Type: x.ActivityType}
			rec := base
			rec.Name, rec.Value, rec.Unit = "Longest", x.Duration, "s"
			consider(x.ActivityType+"/duration", rec, true)
			if x.Distance != nil && *x.Distance > 0 {
				t.Distance += *x.Distance
				rec = base
				rec.Name, rec.Value, rec.Unit = "Farthest", *x.Distance, "m"
				consider(x.ActivityType+"/distance", rec, true)
				if *x.Distance >= 1000 && x.Duration > 0 {
					rec = base
					rec.Name, rec.Value, rec.Unit = "Fastest average pace", x.Duration/(*x.Distance/1000), "s/km"
					consider(x.ActivityType+"/pace", rec, false)
				}
			}
			if x.Energy != nil {
				t.Energy += *x.Energy
				rec = base
				rec.Name, rec.Value, rec.Unit = "Most energy", *x.Energy, "kcal"
				consider(x.ActivityType+"/energy", rec, true)
			}
			if x.Elevation != nil {
				rec = base
				rec.Name, rec.Value, rec.Unit = "Most climbing", *x.Elevation, "m"
				consider(x.ActivityType+"/elevation", rec, true)
			}
		}
		for _, rec := range best {
			recs = append(recs, rec)
		}

		// Best days and health readings from daily series over all time.
		from, to := time.Unix(0, 0).UTC(), time.Now().Add(24*time.Hour).UTC()
		daily := []struct {
			typ, name, group string
			higher           bool
		}{
			{"step_count", "Most steps in a day", "day", true},
			{"distance_walking_running", "Most walking + running distance in a day", "day", true},
			{"active_energy_burned", "Most active energy in a day", "day", true},
			{"flights_climbed", "Most flights climbed in a day", "day", true},
			{"apple_exercise_time", "Most exercise minutes in a day", "day", true},
			{"vo2_max", "Highest VO₂ max", "health", true},
			{"resting_heart_rate", "Lowest resting heart rate", "health", false},
			{"heart_rate_variability_sdnn", "Highest daily HRV", "health", true},
			{"walking_heart_rate_average", "Lowest walking heart rate", "health", false},
		}
		for _, d := range daily {
			s, err := wa.seriesFor(d.typ, from, to, "day", loc)
			if err != nil {
				return nil, err
			}
			if len(s.Points) == 0 {
				continue
			}
			bi := 0
			for i, p := range s.Points {
				if (d.higher && p.Value > s.Points[bi].Value) || (!d.higher && p.Value < s.Points[bi].Value) {
					bi = i
				}
			}
			recs = append(recs, Record{Group: d.group, Name: d.name, Value: s.Points[bi].Value, Unit: s.Unit, When: s.Points[bi].T, Type: d.typ})
		}

		// Ring streaks.
		rows, err := wa.db.Query(`SELECT date, active_energy_kcal >= active_energy_goal_kcal AND active_energy_goal_kcal > 0,
			exercise_min >= exercise_goal_min AND exercise_goal_min > 0, stand_hours >= stand_hours_goal AND stand_hours_goal > 0
			FROM activity_summaries ORDER BY date`)
		if err != nil {
			return nil, err
		}
		type streak struct {
			name      string
			cur, best int
			bestEnd   time.Time
			days      int
		}
		streaks := []*streak{{name: "Move"}, {name: "Exercise"}, {name: "Stand"}, {name: "All three rings"}}
		var prev time.Time
		for rows.Next() {
			var d time.Time
			var m, e, s *bool
			if err := rows.Scan(&d, &m, &e, &s); err != nil {
				rows.Close()
				return nil, err
			}
			gap := !prev.IsZero() && d.Sub(prev) > 36*time.Hour
			prev = d
			closed := []bool{deref(m), deref(e), deref(s), deref(m) && deref(e) && deref(s)}
			for i, st := range streaks {
				if gap {
					st.cur = 0
				}
				if closed[i] {
					st.cur++
					st.days++
					if st.cur > st.best {
						st.best, st.bestEnd = st.cur, d
					}
				} else {
					st.cur = 0
				}
			}
		}
		rows.Close()
		ringStats := []map[string]any{}
		for _, st := range streaks {
			ringStats = append(ringStats, map[string]any{"ring": st.name, "current": st.cur, "longest": st.best,
				"longestEnded": st.bestEnd.Format("2006-01-02"), "daysClosed": st.days})
		}

		tl := make([]*activityTotals, 0, len(totals))
		for _, t := range totals {
			t.Distance, t.Energy = math.Round(t.Distance), math.Round(t.Energy)
			tl = append(tl, t)
		}
		sort.Slice(tl, func(i, j int) bool { return tl[i].Count > tl[j].Count })
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Group != recs[j].Group {
				return recs[i].Group < recs[j].Group
			}
			if recs[i].Type != recs[j].Type {
				return recs[i].Type < recs[j].Type
			}
			return recs[i].Name < recs[j].Name
		})
		return map[string]any{"records": recs, "totals": tl, "rings": ringStats}, nil
	})
}

func deref(b *bool) bool { return b != nil && *b }

type workoutEfforts struct {
	start   int64
	efforts []effort
}

// GET /api/records/efforts?type=running: the fastest 400 m … marathon found anywhere in the GPS routes, with the
// workout each came from. Every route is read once and its result kept in memory.
func (wa *WebAPI) efforts(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	if typ == "" {
		typ = "running"
	}
	wa.cached(w, r, 10*time.Minute, func() (any, error) {
		all, err := wa.workoutList(time.Unix(0, 0), time.Now().Add(24*time.Hour))
		if err != nil {
			return nil, err
		}
		type best struct {
			effort
			UUID  string `json:"uuid"`
			Start int64  `json:"start"`
		}
		bests := map[string]*best{}
		for _, x := range all {
			if x.ActivityType != typ || !x.HasRoute {
				continue
			}
			wa.effortsMu.Lock()
			we, ok := wa.effortCache[x.UUID]
			wa.effortsMu.Unlock()
			if !ok {
				pts, err := wa.routePoints(x.UUID)
				if err != nil {
					return nil, err
				}
				we = workoutEfforts{start: x.Start}
				for _, ed := range effortDistances {
					if sec, ok := bestEffort(pts, ed.m); ok {
						we.efforts = append(we.efforts, effort{ed.name, ed.m, math.Round(sec)})
					}
				}
				wa.effortsMu.Lock()
				wa.effortCache[x.UUID] = we
				wa.effortsMu.Unlock()
			}
			for _, e := range we.efforts {
				if b := bests[e.Name]; b == nil || e.Seconds < b.Seconds {
					bests[e.Name] = &best{e, x.UUID, x.Start}
				}
			}
		}
		out := []*best{}
		for _, ed := range effortDistances {
			if b := bests[ed.name]; b != nil {
				out = append(out, b)
			}
		}
		return out, nil
	})
}

package main

// Activity rings as Metrics types: the daily Move, Exercise and Stand values, their goals and the share of the goal
// reached, read from activity_summaries (one row per day, so they have no samples or hourly rollup).

import (
	"database/sql"
	"strings"
	"time"
)

type ringType struct {
	Name, Unit, Expr string
}

var ringTypes = []ringType{
	{"activity_move", "kcal", "active_energy_kcal"},
	{"activity_move_goal", "kcal", "active_energy_goal_kcal"},
	{"activity_move_percent", "%", "active_energy_kcal / NULLIF(active_energy_goal_kcal, 0)"},
	{"activity_exercise", "min", "exercise_min"},
	{"activity_exercise_goal", "min", "exercise_goal_min"},
	{"activity_exercise_percent", "%", "exercise_min / NULLIF(exercise_goal_min, 0)"},
	{"activity_stand", "h", "stand_hours"},
	{"activity_stand_goal", "h", "stand_hours_goal"},
	{"activity_stand_percent", "%", "stand_hours / NULLIF(stand_hours_goal, 0)"},
}

func ringByName(name string) (ringType, bool) {
	for _, r := range ringTypes {
		if r.Name == name {
			return r, true
		}
	}
	return ringType{}, false
}

// ringTypes lists the ring types when there are activity summaries at all.
func (wa *WebAPI) ringTypes() ([]typeInfo, error) {
	var n int64
	var first, last sql.NullString
	if err := wa.db.QueryRow("SELECT COUNT(*), MIN(date), MAX(date) FROM activity_summaries").Scan(&n, &first, &last); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	day := func(s sql.NullString) int64 {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(s.String)[:min(10, len(strings.TrimSpace(s.String)))])
		if err != nil {
			return 0
		}
		return t.Unix()
	}
	out := make([]typeInfo, 0, len(ringTypes))
	for _, r := range ringTypes {
		out = append(out, typeInfo{Type: r.Name, Kind: "ring", Unit: r.Unit, Count: n, First: day(first), Last: day(last)})
	}
	return out, nil
}

// ringSeries gives one point per day; weeks, months and years average the days.
func (wa *WebAPI) ringSeries(info typeInfo, from, to time.Time, bucket string, loc *time.Location) (*seriesReply, error) {
	r, _ := ringByName(info.Type)
	if bucket == "raw" || bucket == "hour" {
		bucket = "day"
	}
	reply := &seriesReply{Type: info.Type, Unit: info.Unit, Bucket: bucket, Points: []Point{}}
	rows, err := wa.db.Query(`SELECT date, `+r.Expr+` FROM activity_summaries WHERE date >= ? AND date <= ? ORDER BY date`,
		from.In(loc).Format("2006-01-02"), to.Add(-time.Second).In(loc).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hours []hourRow
	for rows.Next() {
		var d string
		var v sql.NullFloat64
		if err := rows.Scan(&d, &v); err != nil {
			return nil, err
		}
		if !v.Valid {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02", d[:min(10, len(d))], loc)
		if err != nil {
			continue
		}
		hours = append(hours, hourRow{Hour: t, Sum: v.Float64, N: 1, Min: v.Float64, Max: v.Float64})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reply.Points = foldHours(hours, bucket, loc, false)
	return reply, nil
}

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tables counted for the ack the phone shows when a sync finishes.
var countedTables = []string{"samples", "workouts", "workout_route_points", "heartbeats", "activity_summaries", "characteristics"}

// batch collects rows for one multi-row upsert.
type batch struct {
	table string
	cols  []string
	rows  [][]any
}

const batchRows = 1000     // rows per INSERT statement
const commitEvery = 50_000 // messages per transaction

// Session applies one phone connection's messages to MariaDB. Not safe for concurrent use; the server runs one at a time.
type Session struct {
	db          *sql.DB
	tx          *sql.Tx
	batches     map[string]*batch
	order       []string
	sinceCommit int
	Applied     int
	logID       int64
}

func NewSession(db *sql.DB) *Session {
	s := &Session{db: db, batches: map[string]*batch{}}
	s.define("samples", "uuid", "type", "kind", "start_date", "end_date", "value", "unit", "text", "count",
		"source_name", "source_bundle", "source_version", "source_product", "source_os", "device_json", "metadata_json")
	s.define("workout_route_points", "workout_uuid", "route_uuid", "time", "latitude", "longitude", "altitude",
		"horizontal_accuracy", "vertical_accuracy", "speed", "course")
	s.define("heartbeats", "series_uuid", "time_since_start_s", "preceded_by_gap")
	s.define("activity_summaries", "date", "active_energy_kcal", "active_energy_goal_kcal", "exercise_min", "exercise_goal_min",
		"stand_hours", "stand_hours_goal", "move_min", "move_goal_min")
	s.define("characteristics", "`key`", "value")
	return s
}

func (s *Session) define(table string, cols ...string) {
	s.batches[table] = &batch{table: table, cols: cols}
	s.order = append(s.order, table)
}

func (s *Session) begin() error {
	if s.tx != nil {
		return nil
	}
	tx, err := s.db.Begin()
	s.tx = tx
	return err
}

func (s *Session) add(table string, row ...any) error {
	b := s.batches[table]
	b.rows = append(b.rows, row)
	if len(b.rows) >= batchRows {
		return s.flushBatch(b)
	}
	return nil
}

func (s *Session) flushBatch(b *batch) error {
	if len(b.rows) == 0 {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	ph := "(" + strings.TrimSuffix(strings.Repeat("?,", len(b.cols)), ",") + ")"
	updates := make([]string, len(b.cols))
	for i, c := range b.cols {
		updates[i] = fmt.Sprintf("%s=VALUES(%s)", c, c)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s ON DUPLICATE KEY UPDATE %s",
		b.table, strings.Join(b.cols, ","), strings.TrimSuffix(strings.Repeat(ph+",", len(b.rows)), ","), strings.Join(updates, ","))
	args := make([]any, 0, len(b.rows)*len(b.cols))
	for _, r := range b.rows {
		args = append(args, r...)
	}
	if _, err := s.tx.Exec(q, args...); err != nil {
		return fmt.Errorf("insert into %s: %w", b.table, err)
	}
	if b.table == "samples" {
		// A re-added object is no longer deleted.
		uuids := make([]any, len(b.rows))
		for i, r := range b.rows {
			uuids[i] = r[0]
		}
		in := strings.TrimSuffix(strings.Repeat("?,", len(uuids)), ",")
		if _, err := s.tx.Exec("DELETE FROM deleted_objects WHERE uuid IN ("+in+")", uuids...); err != nil {
			return err
		}
	}
	b.rows = b.rows[:0]
	return nil
}

func (s *Session) flushAll() error {
	for _, t := range s.order {
		if err := s.flushBatch(s.batches[t]); err != nil {
			return err
		}
	}
	return nil
}

// Commit writes everything buffered so far. Called on done, and on disconnect so a broken sync keeps what arrived.
func (s *Session) Commit() error {
	if err := s.flushAll(); err != nil {
		return err
	}
	if s.tx == nil {
		return nil
	}
	err := s.tx.Commit()
	s.tx = nil
	s.sinceCommit = 0
	return err
}

func (s *Session) Rollback() {
	if s.tx != nil {
		s.tx.Rollback()
		s.tx = nil
	}
}

// Apply handles one message. It returns true for `done`, after which the caller sends the ack.
func (s *Session) Apply(m Message) (bool, error) {
	if err := s.begin(); err != nil {
		return false, err
	}
	switch {
	case m.Hello != nil:
		h := m.Hello
		res, err := s.tx.Exec("INSERT INTO sync_log (started_at, device, `system`, full_resync, items) VALUES (?,?,?,?,0)",
			time.Now().UTC(), h.DeviceName, h.SystemVersion, h.FullResync)
		if err != nil {
			return false, err
		}
		s.logID, _ = res.LastInsertId()

	case m.Characteristic != nil:
		if err := s.add("characteristics", m.Characteristic.Key, m.Characteristic.Value); err != nil {
			return false, err
		}

	case m.Sample != nil:
		x := m.Sample
		if err := s.add("samples", x.UUID, x.Type, x.Kind, x.Start.UTC(), x.End.UTC(), x.Value, x.Unit, x.Text, x.Count,
			x.Source.Name, x.Source.BundleID, x.Source.Version, x.Source.ProductType, x.Source.OSVersion,
			rawJSON(x.Device), mapJSON(x.Metadata)); err != nil {
			return false, err
		}

	case m.Workout != nil:
		if err := s.applyWorkout(m.Workout); err != nil {
			return false, err
		}

	case m.RoutePoint != nil:
		p := m.RoutePoint
		if err := s.add("workout_route_points", p.WorkoutUUID, p.RouteUUID, p.Time.UTC(), p.Latitude, p.Longitude, p.Altitude,
			p.HorizontalAccuracy, p.VerticalAccuracy, p.Speed, p.Course); err != nil {
			return false, err
		}

	case m.Heartbeat != nil:
		h := m.Heartbeat
		if err := s.add("heartbeats", h.SeriesUUID, h.TimeSinceStart, h.PrecededByGap); err != nil {
			return false, err
		}

	case m.ActivitySummary != nil:
		a := m.ActivitySummary
		if err := s.add("activity_summaries", a.Date, a.ActiveEnergyBurned, a.ActiveEnergyBurnedGoal, a.AppleExerciseTime,
			a.AppleExerciseTimeGoal, a.AppleStandHours, a.AppleStandHoursGoal, a.AppleMoveTime, a.AppleMoveTimeGoal); err != nil {
			return false, err
		}

	case m.Deleted != nil:
		// Buffered inserts first, so a delete always wins over an earlier add of the same object.
		if err := s.flushAll(); err != nil {
			return false, err
		}
		d := m.Deleted
		for _, q := range []string{"DELETE FROM samples WHERE uuid = ?", "DELETE FROM workouts WHERE uuid = ?"} {
			if _, err := s.tx.Exec(q, d.UUID); err != nil {
				return false, err
			}
		}
		if _, err := s.tx.Exec("REPLACE INTO deleted_objects VALUES (?,?,?)", d.UUID, d.Type, time.Now().UTC()); err != nil {
			return false, err
		}

	case m.Done != nil:
		if err := s.flushAll(); err != nil {
			return false, err
		}
		if s.logID != 0 {
			if _, err := s.tx.Exec("UPDATE sync_log SET finished_at = ?, items = ? WHERE id = ?", time.Now().UTC(), m.Done.TotalSent, s.logID); err != nil {
				return false, err
			}
		}
		return true, s.Commit()

	default: // typeDone, ignored ECG messages
		return false, nil
	}

	s.Applied++
	s.sinceCommit++
	if s.sinceCommit >= commitEvery {
		return false, s.Commit()
	}
	return false, nil
}

func (s *Session) applyWorkout(w *Workout) error {
	if err := s.flushAll(); err != nil {
		return err
	}
	_, err := s.tx.Exec(`REPLACE INTO workouts (uuid, activity_type, activity_type_raw, start_date, end_date, duration_s,
		source_name, source_bundle, source_version, source_product, source_os, device_json, metadata_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.UUID, w.ActivityType, w.ActivityTypeRaw, w.Start.UTC(), w.End.UTC(), w.Duration,
		w.Source.Name, w.Source.BundleID, w.Source.Version, w.Source.ProductType, w.Source.OSVersion, rawJSON(w.Device), mapJSON(w.Metadata))
	if err != nil {
		return err
	}
	for _, q := range []string{"DELETE FROM workout_events WHERE workout_uuid = ?", "DELETE FROM workout_activities WHERE workout_uuid = ?"} {
		if _, err := s.tx.Exec(q, w.UUID); err != nil {
			return err
		}
	}
	for _, st := range w.Statistics {
		if _, err := s.tx.Exec("REPLACE INTO workout_statistics VALUES (?,?,?,?,?,?,?,?)",
			w.UUID, st.Type, st.Unit, st.Average, st.Minimum, st.Maximum, st.Sum, st.MostRecent); err != nil {
			return err
		}
	}
	for _, e := range w.Events {
		if _, err := s.tx.Exec("INSERT INTO workout_events VALUES (?,?,?,?,?)", w.UUID, e.Type, e.Start.UTC(), e.End.UTC(), mapJSON(e.Metadata)); err != nil {
			return err
		}
	}
	for _, a := range w.Activities {
		var end any
		if a.End != nil {
			end = a.End.UTC()
		}
		if _, err := s.tx.Exec("INSERT INTO workout_activities VALUES (?,?,?,?,?)", w.UUID, a.ActivityType, a.Start.UTC(), end, mapJSON(a.Metadata)); err != nil {
			return err
		}
	}
	return nil
}

func TotalRows(db *sql.DB) (int, error) {
	total := 0
	for _, t := range countedTables {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func rawJSON(r json.RawMessage) any {
	if len(r) == 0 || string(r) == "null" {
		return nil
	}
	return string(r)
}

func mapJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

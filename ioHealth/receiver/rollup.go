package main

// sample_hourly holds every type's samples summed per UTC hour and source, so charts and records over months or years
// read a few thousand rows instead of millions. It is kept up to date inside each upload batch's transaction, and
// filled for data that predates it by a background job that works one type and one month at a time (small
// transactions, which a Galera cluster replicates without trouble). Until a type is filled, its charts read samples.

import (
	"database/sql"
	"log"
	"sort"
	"sync"
	"time"
)

// rollupMu keeps the backfill and upload batches from recomputing the same hours at once.
var rollupMu sync.Mutex

// hourlyValue is what a sample adds to its hour: its value, or for category samples (mindful sessions, stand hours)
// its duration in minutes.
const hourlyValue = "CASE WHEN kind = 'category' THEN TIMESTAMPDIFF(SECOND, start_date, end_date) / 60 ELSE value END"

type typeHour struct {
	Type string
	Hour time.Time
}

// touched collects the hours an upload batch changed.
type touched map[typeHour]bool

func (t touched) add(typ string, start time.Time) {
	t[typeHour{typ, start.UTC().Truncate(time.Hour)}] = true
}

// hourSpans groups each type's hours into runs with no gap over a day, so a batch with samples years apart
// recomputes a few short spans rather than everything between them.
func (t touched) hourSpans() map[string][][2]time.Time {
	byType := map[string][]time.Time{}
	for k := range t {
		byType[k.Type] = append(byType[k.Type], k.Hour)
	}
	out := map[string][][2]time.Time{}
	for typ, hours := range byType {
		sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
		start, last := hours[0], hours[0]
		for _, h := range hours[1:] {
			if h.Sub(last) > 24*time.Hour {
				out[typ] = append(out[typ], [2]time.Time{start, last.Add(time.Hour)})
				start = h
			}
			last = h
		}
		out[typ] = append(out[typ], [2]time.Time{start, last.Add(time.Hour)})
	}
	return out
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// recomputeHours rebuilds sample_hourly for one type over [from, to), both on hour boundaries.
func recomputeHours(x execer, typ string, from, to time.Time) error {
	if _, err := x.Exec("DELETE FROM sample_hourly WHERE type = ? AND hour >= ? AND hour < ?", typ, from, to); err != nil {
		return err
	}
	_, err := x.Exec(`INSERT INTO sample_hourly (type, hour, source_name, sum, n, min, max)
		SELECT type, DATE_FORMAT(start_date, '%Y-%m-%d %H:00:00') AS h, COALESCE(source_name, ''),
			SUM(`+hourlyValue+`), COUNT(*), MIN(`+hourlyValue+`), MAX(`+hourlyValue+`)
		FROM samples WHERE type = ? AND start_date >= ? AND start_date < ? AND (value IS NOT NULL OR kind = 'category')
		GROUP BY h, COALESCE(source_name, '')`, typ, from, to)
	return err
}

// rollupReady is the set of types whose sample_hourly rows are complete.
type rollupReady struct {
	mu    sync.RWMutex
	types map[string]bool
}

func (r *rollupReady) has(t string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.types[t]
}

func (r *rollupReady) set(t string) {
	r.mu.Lock()
	r.types[t] = true
	r.mu.Unlock()
}

// backfillHourly fills sample_hourly for every type not yet marked ready, a month per transaction, and marks each
// type ready in sample_hourly_ready when it's done. Safe to interrupt: a restart redoes the unfinished type.
func backfillHourly(db *sql.DB, ready *rollupReady, done func()) {
	rows, err := db.Query("SELECT type FROM sample_hourly_ready")
	if err != nil {
		log.Printf("hourly rollup: %v", err)
		return
	}
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			ready.set(t)
		}
	}
	rows.Close()

	rows, err = db.Query("SELECT type, MIN(start_date), MAX(start_date) FROM samples GROUP BY type")
	if err != nil {
		log.Printf("hourly rollup: %v", err)
		return
	}
	type span struct {
		typ      string
		from, to time.Time
	}
	var todo []span
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.typ, &s.from, &s.to); err == nil && !ready.has(s.typ) {
			todo = append(todo, s)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	start := time.Now()
	log.Printf("hourly rollup: filling %d types in the background", len(todo))
	for _, s := range todo {
		from := time.Date(s.from.Year(), s.from.Month(), 1, 0, 0, 0, 0, time.UTC)
		for from.Before(s.to) || from.Equal(s.to) {
			to := from.AddDate(0, 1, 0)
			rollupMu.Lock()
			err := func() error {
				tx, err := db.Begin()
				if err != nil {
					return err
				}
				if err := recomputeHours(tx, s.typ, from, to); err != nil {
					tx.Rollback()
					return err
				}
				return tx.Commit()
			}()
			rollupMu.Unlock()
			if err != nil {
				log.Printf("hourly rollup: %s %s: %v (will retry on next start)", s.typ, from.Format("2006-01"), err)
				return
			}
			from = to
		}
		if _, err := db.Exec("INSERT IGNORE INTO sample_hourly_ready (type) VALUES (?)", s.typ); err != nil {
			log.Printf("hourly rollup: %v", err)
			return
		}
		ready.set(s.typ)
		if done != nil {
			done()
		}
	}
	log.Printf("hourly rollup: done in %s", time.Since(start).Round(time.Second))
}

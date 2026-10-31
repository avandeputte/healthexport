package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

const tableOptions = " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"

// migration is a list of statements, and optionally Go code run after them.
type migration struct {
	stmts []string
	run   func(*sql.DB) error
}

// migrations are applied in order, once each, and recorded in schema_version. To change the schema, append a new
// entry; never edit one that has shipped. Timestamps are UTC.
var migrations = []migration{
	// 1: the Mac app's SQLite tables (Mac/SyncDatabase.swift), minus ECGs.
	{stmts: []string{
		"CREATE TABLE IF NOT EXISTS characteristics (`key` VARCHAR(64) PRIMARY KEY, value TEXT)",
		`CREATE TABLE IF NOT EXISTS samples (
			uuid CHAR(36) PRIMARY KEY,
			type VARCHAR(128) NOT NULL,
			kind VARCHAR(32) NOT NULL,
			start_date DATETIME(3) NOT NULL,
			end_date DATETIME(3) NOT NULL,
			value DOUBLE, unit VARCHAR(64), text MEDIUMTEXT, count INT,
			source_name VARCHAR(255), source_bundle VARCHAR(255), source_version VARCHAR(64), source_product VARCHAR(64), source_os VARCHAR(32),
			device_json JSON, metadata_json JSON,
			INDEX idx_samples_type_start (type, start_date))`,
		`CREATE TABLE IF NOT EXISTS workouts (
			uuid CHAR(36) PRIMARY KEY,
			activity_type VARCHAR(64), activity_type_raw INT,
			start_date DATETIME(3), end_date DATETIME(3), duration_s DOUBLE,
			source_name VARCHAR(255), source_bundle VARCHAR(255), source_version VARCHAR(64), source_product VARCHAR(64), source_os VARCHAR(32),
			device_json JSON, metadata_json JSON,
			INDEX idx_workouts_start (start_date))`,
		`CREATE TABLE IF NOT EXISTS workout_statistics (
			workout_uuid CHAR(36), type VARCHAR(128), unit VARCHAR(64),
			average DOUBLE, minimum DOUBLE, maximum DOUBLE, sum DOUBLE, most_recent DOUBLE,
			PRIMARY KEY (workout_uuid, type))`,
		`CREATE TABLE IF NOT EXISTS workout_events (
			workout_uuid CHAR(36), type VARCHAR(64), start_date DATETIME(3), end_date DATETIME(3), metadata_json JSON,
			INDEX idx_events_workout (workout_uuid))`,
		`CREATE TABLE IF NOT EXISTS workout_activities (
			workout_uuid CHAR(36), activity_type VARCHAR(64), start_date DATETIME(3), end_date DATETIME(3), metadata_json JSON,
			INDEX idx_activities_workout (workout_uuid))`,
		`CREATE TABLE IF NOT EXISTS workout_route_points (
			workout_uuid CHAR(36), route_uuid CHAR(36), time DATETIME(3),
			latitude DOUBLE, longitude DOUBLE, altitude DOUBLE, horizontal_accuracy DOUBLE, vertical_accuracy DOUBLE, speed DOUBLE, course DOUBLE,
			PRIMARY KEY (route_uuid, time),
			INDEX idx_route_points_workout (workout_uuid))`,
		`CREATE TABLE IF NOT EXISTS heartbeats (
			series_uuid CHAR(36), time_since_start_s DOUBLE, preceded_by_gap BOOL,
			PRIMARY KEY (series_uuid, time_since_start_s))`,
		`CREATE TABLE IF NOT EXISTS activity_summaries (
			date DATE PRIMARY KEY,
			active_energy_kcal DOUBLE, active_energy_goal_kcal DOUBLE,
			exercise_min DOUBLE, exercise_goal_min DOUBLE,
			stand_hours DOUBLE, stand_hours_goal DOUBLE,
			move_min DOUBLE, move_goal_min DOUBLE)`,
		"CREATE TABLE IF NOT EXISTS deleted_objects (uuid CHAR(36) PRIMARY KEY, type VARCHAR(128), deleted_at DATETIME(3))",
		"CREATE TABLE IF NOT EXISTS sync_log (" +
			"id INT AUTO_INCREMENT PRIMARY KEY, started_at DATETIME(3), finished_at DATETIME(3), " +
			"device VARCHAR(255), `system` VARCHAR(32), full_resync BOOL, items INT)",
	}},
	// 2: HTTP uploads come in numbered batches; remembering the last one makes a retried batch a no-op.
	{stmts: []string{
		"ALTER TABLE sync_log ADD COLUMN IF NOT EXISTS last_batch_seq INT NOT NULL DEFAULT 0",
	}},
	// 3: the phone only sends the source's name now; drop the columns nobody used.
	{stmts: []string{
		"ALTER TABLE samples DROP COLUMN IF EXISTS source_bundle, DROP COLUMN IF EXISTS source_version, " +
			"DROP COLUMN IF EXISTS source_product, DROP COLUMN IF EXISTS source_os, DROP COLUMN IF EXISTS device_json",
		"ALTER TABLE workouts DROP COLUMN IF EXISTS source_bundle, DROP COLUMN IF EXISTS source_version, " +
			"DROP COLUMN IF EXISTS source_product, DROP COLUMN IF EXISTS source_os, DROP COLUMN IF EXISTS device_json",
	}},
	// 4: types are stored as short snake_case names (heart_rate, not HKQuantityTypeIdentifierHeartRate).
	{run: renameTypes},
	// 5: when each row arrived (inserted_at) and when its values last changed (updated_at), so the lag between a
	// measurement and its upload can be measured. Rows that existed before this migration get NULL: nobody knows.
	{stmts: timestampColumns(
		"samples", "workouts", "workout_statistics", "workout_events", "workout_activities", "workout_route_points",
		"heartbeats", "activity_summaries", "characteristics", "deleted_objects")},
	// 6: views on how long samples take to reach the database (inserted_at minus the sample's end), by type and by
	// type per day. CREATE OR REPLACE, so a later migration can redefine them.
	{stmts: []string{
		`CREATE OR REPLACE VIEW sample_delay_by_type AS
		SELECT type,
			COUNT(*) AS samples,
			ROUND(MIN(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS min_delay_min,
			ROUND(AVG(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS avg_delay_min,
			ROUND(MAX(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS max_delay_min,
			MAX(end_date) AS latest_sample,
			MAX(inserted_at) AS latest_arrival
		FROM samples WHERE inserted_at IS NOT NULL
		GROUP BY type`,
		`CREATE OR REPLACE VIEW sample_delay_by_day AS
		SELECT DATE(inserted_at) AS day, type,
			COUNT(*) AS samples,
			ROUND(MIN(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS min_delay_min,
			ROUND(AVG(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS avg_delay_min,
			ROUND(MAX(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60, 1) AS max_delay_min
		FROM samples WHERE inserted_at IS NOT NULL
		GROUP BY DATE(inserted_at), type`,
	}},
	// 7: a covering index for the delay views, so they read the index instead of the whole samples table and skip
	// the rows without inserted_at outright.
	{stmts: []string{
		"ALTER TABLE samples ADD INDEX IF NOT EXISTS idx_samples_inserted (inserted_at, type, end_date)",
	}},
	// 8: for the web app. sample_hourly holds samples summed per type, UTC hour and source (see rollup.go), so long
	// charts and records don't scan millions of samples; it is filled in the background, a month at a time, and
	// sample_hourly_ready lists the types that are complete. web_settings keeps the web app's settings.
	{stmts: []string{
		`CREATE TABLE IF NOT EXISTS sample_hourly (
			type VARCHAR(128) NOT NULL, hour DATETIME NOT NULL, source_name VARCHAR(255) NOT NULL DEFAULT '',
			sum DOUBLE, n INT NOT NULL, min DOUBLE, max DOUBLE,
			PRIMARY KEY (type, hour, source_name))`,
		"CREATE TABLE IF NOT EXISTS sample_hourly_ready (type VARCHAR(128) PRIMARY KEY)",
		"CREATE TABLE IF NOT EXISTS web_settings (`key` VARCHAR(64) PRIMARY KEY, value MEDIUMTEXT NOT NULL, " +
			"updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3))",
	}},
}

// timestampColumns adds inserted_at/updated_at to the tables. The columns are added without a default first and
// the defaults set afterwards, so existing rows stay NULL without a row-by-row UPDATE (which on a Galera cluster is
// one huge replicated transaction, enough to knock a node out). The session time zone is UTC (main.go), so
// CURRENT_TIMESTAMP is UTC like every other timestamp here.
func timestampColumns(tables ...string) []string {
	var stmts []string
	for _, t := range tables {
		stmts = append(stmts,
			"ALTER TABLE "+t+" ADD COLUMN IF NOT EXISTS inserted_at DATETIME(3) NULL DEFAULT NULL, "+
				"ADD COLUMN IF NOT EXISTS updated_at DATETIME(3) NULL DEFAULT NULL",
			"ALTER TABLE "+t+" MODIFY COLUMN inserted_at DATETIME(3) NULL DEFAULT CURRENT_TIMESTAMP(3), "+
				"MODIFY COLUMN updated_at DATETIME(3) NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)")
	}
	return stmts
}

func schemaVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&v)
	return v, err
}

// createDatabase makes the database if the user is allowed to; if not, it must already exist.
func createDatabase(db *sql.DB, name string) {
	q := "CREATE DATABASE IF NOT EXISTS `" + strings.ReplaceAll(name, "`", "``") + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	if _, err := db.Exec(q); err != nil {
		log.Printf("couldn't create database %s (fine if it already exists): %v", name, err)
	}
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INT PRIMARY KEY, applied_at DATETIME(3) NOT NULL)" + tableOptions); err != nil {
		return err
	}
	var current int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		version := i + 1
		for _, stmt := range migrations[i].stmts {
			if strings.HasPrefix(stmt, "CREATE TABLE") {
				stmt += tableOptions
			}
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("migration %d: %w\n%s", version, err, stmt)
			}
		}
		if run := migrations[i].run; run != nil {
			if err := run(db); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
		}
		if _, err := db.Exec("INSERT INTO schema_version VALUES (?, UTC_TIMESTAMP(3))", version); err != nil {
			return err
		}
		log.Printf("applied schema migration %d", version)
	}
	return nil
}

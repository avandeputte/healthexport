package main

import (
	"database/sql"
	"log"
	"regexp"
	"strings"
)

var typePrefixes = []string{"HKQuantityTypeIdentifier", "HKCategoryTypeIdentifier", "HKCharacteristicTypeIdentifier",
	"HKDataTypeIdentifier"}

var (
	lowerThenUpper  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	acronymThenWord = regexp.MustCompile(`([A-Z])([A-Z][a-z])`)
)

// storageName turns a HealthKit identifier into the short name stored in `type` columns, the same rule as
// HealthTypes.storageName on the phone: "HKQuantityTypeIdentifierHeartRate" -> "heart_rate",
// "...VO2Max" -> "vo2_max", "...HeartRateVariabilitySDNN" -> "heart_rate_variability_sdnn".
func storageName(id string) string {
	name := id
	for _, p := range typePrefixes {
		if strings.HasPrefix(name, p) {
			name = strings.TrimPrefix(name, p)
			break
		}
	}
	// "HKWorkoutTypeIdentifier" -> "Workout", "HKActivitySummaryTypeIdentifier" -> "ActivitySummary".
	if strings.HasPrefix(name, "HK") {
		name = name[2:]
		if strings.HasSuffix(name, "TypeIdentifier") && len(name) > len("TypeIdentifier") {
			name = strings.TrimSuffix(name, "TypeIdentifier")
		}
	}
	// Go's regexp has no lookaround, so apply each split until nothing changes ("SDNNIndex" needs two passes).
	for _, re := range []*regexp.Regexp{lowerThenUpper, acronymThenWord} {
		for {
			next := re.ReplaceAllString(name, "${1}_${2}")
			if next == name {
				break
			}
			name = next
		}
	}
	return strings.ToLower(name)
}

// renameTypes rewrites rows stored with full HealthKit identifiers, one type at a time, each in its own
// transaction. Rows already renamed don't start with "HK" and are left alone, so it can be rerun safely.
func renameTypes(db *sql.DB) error {
	for _, table := range []string{"samples", "workout_statistics", "deleted_objects"} {
		rows, err := db.Query("SELECT DISTINCT type FROM " + table + " WHERE type LIKE BINARY 'HK%'")
		if err != nil {
			return err
		}
		var types []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			types = append(types, t)
		}
		rows.Close()
		for _, t := range types {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			res, err := tx.Exec("UPDATE "+table+" SET type = ? WHERE type = ?", storageName(t), t)
			if err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			log.Printf("renamed %s.type %s -> %s (%d rows)", table, t, storageName(t), n)
		}
	}
	return nil
}

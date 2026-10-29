package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestStorageName(t *testing.T) {
	cases := map[string]string{
		"HKQuantityTypeIdentifierHeartRate":                "heart_rate",
		"HKQuantityTypeIdentifierDistanceWalkingRunning":   "distance_walking_running",
		"HKCategoryTypeIdentifierSleepAnalysis":            "sleep_analysis",
		"HKCategoryTypeIdentifierAppleStandHour":           "apple_stand_hour",
		"HKQuantityTypeIdentifierVO2Max":                   "vo2_max",
		"HKQuantityTypeIdentifierHeartRateVariabilitySDNN": "heart_rate_variability_sdnn",
		"HKQuantityTypeIdentifierUVExposure":               "uv_exposure",
		"HKQuantityTypeIdentifierForcedExpiratoryVolume1":  "forced_expiratory_volume1",
		"HKDataTypeIdentifierHeartbeatSeries":              "heartbeat_series",
		"HKWorkoutTypeIdentifier":                          "workout",
		"HKWorkoutRouteTypeIdentifier":                     "workout_route",
		"HKActivitySummaryTypeIdentifier":                  "activity_summary",
		"heart_rate":                                       "heart_rate",
	}
	for in, want := range cases {
		if got := storageName(in); got != want {
			t.Errorf("storageName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMatchesPhone compares against the phone's HealthTypes.storageName output, when NAMES_TSV points to it.
func TestMatchesPhone(t *testing.T) {
	path := os.Getenv("NAMES_TSV")
	if path == "" {
		t.Skip("NAMES_TSV not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		id, want, _ := strings.Cut(sc.Text(), "\t")
		if got := storageName(id); got != want {
			t.Errorf("%s: Go %q, phone %q", id, got, want)
		}
		n++
	}
	t.Logf("%d identifiers match", n)
}

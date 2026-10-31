package main

import (
	"math"
	"testing"
	"time"
)

func TestSummarizeGlucose(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rs []glucoseReading
	// One day at 5-minute intervals: 100 mg/dL, except 01:00–01:30 at 60 (a 30-minute low) and 12:00–14:00 at 200.
	for i := 0; i < 288; i++ {
		ts := start.Add(time.Duration(i) * 5 * time.Minute)
		v := 100.0
		switch {
		case ts.Hour() == 1 && ts.Minute() <= 30:
			v = 60
		case ts.Hour() >= 12 && ts.Hour() < 14:
			v = 200
		}
		rs = append(rs, glucoseReading{T: ts, MgDL: v})
	}
	st, agp, days := summarizeGlucose(rs, loc, glucoseRanges{70, 180}, 15)
	if st.Readings != 288 || st.Days != 1 || len(days) != 1 || len(agp) != 96 {
		t.Fatalf("%+v, %d bins, %d days", st, len(agp), len(days))
	}
	if st.Lows != 1 {
		t.Errorf("lows %d", st.Lows)
	}
	if math.Abs(st.Ranges.Low-7.0/288) > 1e-9 || math.Abs(st.Ranges.High-24.0/288) > 1e-9 {
		t.Errorf("ranges %+v", st.Ranges)
	}
	if math.Abs(st.GMI-(3.31+0.02392*st.Mean)) > 1e-9 || st.Min != 60 || st.Max != 200 {
		t.Errorf("%+v", st)
	}
	if agp[48].P50 != 200 || agp[0].P50 != 100 {
		t.Errorf("agp %+v %+v", agp[0], agp[48])
	}
	if toMgDL(5.5, "mmol<180.15588000005408>/L") < 99 || toMgDL(99, "mg/dL") != 99 {
		t.Error("units")
	}
}

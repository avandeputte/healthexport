package main

// Blood glucose: the summary a CGM report gives (time in range, GMI, variability, the ambulatory glucose profile and
// per-day figures). Everything is computed in mg/dL; the browser converts to mmol/L when asked.

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const mgdlPerMmol = 18.0182

type glucoseReading struct {
	T      time.Time
	MgDL   float64
	Source string
	Meal   string // "before meal" / "after meal" from HKBloodGlucoseMealTime, for finger-pricks
}

// toMgDL converts a stored blood glucose value; HealthKit writes mmol/L as "mmol<180.15588...>/L".
func toMgDL(v float64, unit string) float64 {
	if strings.HasPrefix(strings.ToLower(unit), "mmol") {
		return v * mgdlPerMmol
	}
	return v
}

// glucoseRanges are the consensus CGM bands (Battelino et al., Diabetes Care 2019), in mg/dL, with the target range
// configurable. Very low < 54 ≤ low < targetLow ≤ in range ≤ targetHigh < high ≤ 250 < very high.
type glucoseRanges struct{ Low, High float64 }

type rangeShares struct {
	VeryLow  float64 `json:"veryLow"`
	Low      float64 `json:"low"`
	InRange  float64 `json:"inRange"`
	High     float64 `json:"high"`
	VeryHigh float64 `json:"veryHigh"`
}

func (g glucoseRanges) shares(vals []float64) rangeShares {
	var r rangeShares
	if len(vals) == 0 {
		return r
	}
	for _, v := range vals {
		switch {
		case v < 54:
			r.VeryLow++
		case v < g.Low:
			r.Low++
		case v <= g.High:
			r.InRange++
		case v <= 250:
			r.High++
		default:
			r.VeryHigh++
		}
	}
	n := float64(len(vals))
	r.VeryLow, r.Low, r.InRange, r.High, r.VeryHigh = r.VeryLow/n, r.Low/n, r.InRange/n, r.High/n, r.VeryHigh/n
	return r
}

type glucoseStats struct {
	Readings int         `json:"readings"`
	Days     int         `json:"days"`   // days with at least one reading
	PerDay   float64     `json:"perDay"` // readings per day with data; ~288 for a 5-minute CGM
	Mean     float64     `json:"mean"`
	SD       float64     `json:"sd"`
	CV       float64     `json:"cv"`  // % (stable glucose is ≤ 36 %)
	GMI      float64     `json:"gmi"` // glucose management indicator, % (an estimated HbA1c)
	Min      float64     `json:"min"`
	Max      float64     `json:"max"`
	Median   float64     `json:"median"`
	Ranges   rangeShares `json:"ranges"`
	Lows     int         `json:"lows"` // episodes below the target that last 15 minutes or more
}

type agpBin struct {
	Minute int     `json:"minute"` // start of the bin, minutes after local midnight
	N      int     `json:"n"`
	P5     float64 `json:"p5"`
	P25    float64 `json:"p25"`
	P50    float64 `json:"p50"`
	P75    float64 `json:"p75"`
	P95    float64 `json:"p95"`
}

type glucoseDay struct {
	Date   string      `json:"date"`
	N      int         `json:"n"`
	Mean   float64     `json:"mean"`
	Min    float64     `json:"min"`
	Max    float64     `json:"max"`
	SD     float64     `json:"sd"`
	Ranges rangeShares `json:"ranges"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	x := p * float64(len(sorted)-1)
	i := int(x)
	if i+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[i] + (sorted[i+1]-sorted[i])*(x-float64(i))
}

func meanSD(vals []float64) (mean, sd float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	for _, v := range vals {
		sd += (v - mean) * (v - mean)
	}
	if len(vals) > 1 {
		sd = math.Sqrt(sd / float64(len(vals)-1))
	}
	return mean, sd
}

// summarizeGlucose expects readings sorted by time.
func summarizeGlucose(rs []glucoseReading, loc *time.Location, g glucoseRanges, binMinutes int) (glucoseStats, []agpBin, []glucoseDay) {
	var st glucoseStats
	if len(rs) == 0 {
		return st, []agpBin{}, []glucoseDay{}
	}
	all := make([]float64, len(rs))
	bins := make([][]float64, 24*60/binMinutes)
	byDay := map[string][]float64{}
	for i, r := range rs {
		all[i] = r.MgDL
		lt := r.T.In(loc)
		bins[(lt.Hour()*60+lt.Minute())/binMinutes] = append(bins[(lt.Hour()*60+lt.Minute())/binMinutes], r.MgDL)
		d := lt.Format("2006-01-02")
		byDay[d] = append(byDay[d], r.MgDL)
	}
	st.Readings = len(rs)
	st.Days = len(byDay)
	st.PerDay = float64(len(rs)) / float64(st.Days)
	st.Mean, st.SD = meanSD(all)
	if st.Mean > 0 {
		st.CV = st.SD / st.Mean * 100
	}
	st.GMI = 3.31 + 0.02392*st.Mean
	st.Ranges = g.shares(all)
	sorted := append([]float64(nil), all...)
	sort.Float64s(sorted)
	st.Min, st.Max, st.Median = sorted[0], sorted[len(sorted)-1], percentile(sorted, .5)

	// Low episodes: consecutive readings below target spanning at least 15 minutes (gaps over 30 minutes split them).
	var start, prev time.Time
	in := false
	closeEpisode := func() {
		if in && prev.Sub(start) >= 15*time.Minute {
			st.Lows++
		}
		in = false
	}
	for _, r := range rs {
		if r.MgDL < g.Low {
			if in && r.T.Sub(prev) > 30*time.Minute {
				closeEpisode()
			}
			if !in {
				in, start = true, r.T
			}
		} else {
			closeEpisode()
		}
		prev = r.T
	}
	closeEpisode()

	agp := make([]agpBin, 0, len(bins))
	for i, b := range bins {
		if len(b) == 0 {
			continue
		}
		sort.Float64s(b)
		agp = append(agp, agpBin{Minute: i * binMinutes, N: len(b), P5: percentile(b, .05), P25: percentile(b, .25),
			P50: percentile(b, .5), P75: percentile(b, .75), P95: percentile(b, .95)})
	}
	days := make([]glucoseDay, 0, len(byDay))
	for d, vals := range byDay {
		m, sd := meanSD(vals)
		mn, mx := vals[0], vals[0]
		for _, v := range vals {
			mn, mx = math.Min(mn, v), math.Max(mx, v)
		}
		days = append(days, glucoseDay{Date: d, N: len(vals), Mean: m, SD: sd, Min: mn, Max: mx, Ranges: g.shares(vals)})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return st, agp, days
}

// GET /api/glucose?from&to&low&high (mg/dL, default 70–180).
func (wa *WebAPI) glucose(w http.ResponseWriter, r *http.Request) {
	wa.cached(w, r, 0, func() (any, error) {
		loc := location(r)
		from, to, err := timeRange(r, loc, 14)
		if err != nil {
			return nil, err
		}
		g := glucoseRanges{70, 180}
		if v, err := strconv.ParseFloat(r.URL.Query().Get("low"), 64); err == nil && v >= 40 && v <= 120 {
			g.Low = v
		}
		if v, err := strconv.ParseFloat(r.URL.Query().Get("high"), 64); err == nil && v > g.Low && v <= 300 {
			g.High = v
		}
		rows, err := wa.db.Query(`SELECT start_date, value, COALESCE(unit, ''), COALESCE(source_name, ''),
			COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata_json, '$.HKBloodGlucoseMealTime')), '')
			FROM samples WHERE type = 'blood_glucose' AND start_date >= ? AND start_date < ? AND value IS NOT NULL
			ORDER BY start_date`, from, to)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var rs []glucoseReading
		units := map[string]int{}
		sources := map[string]int{}
		for rows.Next() {
			var x glucoseReading
			var v float64
			var unit, meal string
			if err := rows.Scan(&x.T, &v, &unit, &x.Source, &meal); err != nil {
				return nil, err
			}
			x.MgDL = toMgDL(v, unit)
			switch meal {
			case "1":
				x.Meal = "before meal"
			case "2":
				x.Meal = "after meal"
			}
			units[unit]++
			sources[x.Source]++
			rs = append(rs, x)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		stats, agp, days := summarizeGlucose(rs, loc, g, 15)

		// The unit the data was recorded in, so the page can default to it.
		recorded, most := "mg/dL", 0
		for u, n := range units {
			if n > most {
				most = n
				if strings.HasPrefix(strings.ToLower(u), "mmol") {
					recorded = "mmol/L"
				} else {
					recorded = "mg/dL"
				}
			}
		}
		// Raw readings for the chart: everything up to ~50k points, otherwise one per 15 minutes.
		readings := [][]any{}
		step := time.Duration(0)
		if len(rs) > 50000 {
			step = 15 * time.Minute
		}
		var last time.Time
		for _, x := range rs {
			if step > 0 && x.T.Sub(last) < step {
				continue
			}
			last = x.T
			row := []any{x.T.Unix(), math.Round(x.MgDL*10) / 10}
			if x.Meal != "" {
				row = append(row, x.Meal)
			}
			readings = append(readings, row)
		}
		return map[string]any{"unit": "mg/dL", "recordedUnit": recorded, "target": []float64{g.Low, g.High},
			"stats": stats, "agp": agp, "days": days, "readings": readings, "sources": sources}, nil
	})
}

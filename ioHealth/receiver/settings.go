package main

// The web app's settings (units, theme, hidden sections and metrics, heart rate and glucose targets), one row per
// setting in web_settings with the value as JSON, so every browser shares them.

import (
	"encoding/json"
	"errors"
	"net/http"
)

// GET /api/settings returns {key: value}.
func (wa *WebAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := wa.db.Query("SELECT `key`, value FROM web_settings")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if json.Valid([]byte(v)) {
			out[k] = json.RawMessage(v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT /api/settings stores each key of a JSON object; keys not sent are left alone.
func (wa *WebAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var in map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("too many settings"))
		return
	}
	tx, err := wa.db.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for k, v := range in {
		if len(k) == 0 || len(k) > 64 {
			tx.Rollback()
			writeError(w, http.StatusBadRequest, errors.New("setting names are 1 to 64 characters"))
			return
		}
		if _, err := tx.Exec("INSERT INTO web_settings (`key`, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", k, string(v)); err != nil {
			tx.Rollback()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"saved": len(in)})
}

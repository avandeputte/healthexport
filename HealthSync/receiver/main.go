// Command healthsync-receiver accepts Health Sync uploads from the iPhone app over HTTP and writes them to MariaDB,
// and serves a fitness web app (dashboards, charts, workout maps) on the same port. It serves plain HTTP for a reverse proxy that terminates TLS, or HTTPS itself when TLS_CERT_FILE and
// TLS_KEY_FILE are set. The API is described in README.md.
package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; the web app buckets by the browser's time zone

	"github.com/go-sql-driver/mysql"
)

const maxBody = 64 << 20 // per request, after decompression

func main() {
	// `receiver healthcheck` is the container's HEALTHCHECK; the distroless image has no curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if os.Getenv("DB_HOST") == "" {
		log.Fatal("DB_HOST is not set; see .env.example")
	}
	token := os.Getenv("SYNC_TOKEN")
	if len(token) < 16 {
		log.Fatal("SYNC_TOKEN must be a random string of at least 16 characters (openssl rand -hex 32); see .env.example")
	}

	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = env("DB_HOST", "") + ":" + env("DB_PORT", "3306")
	cfg.User = env("DB_USER", "health")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	// Connect without a database first so it can be created if missing.
	dbName := env("DB_NAME", "health")
	cfg.DBName = ""
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		log.Fatal(err)
	}
	// The server may still be starting when the container comes up.
	for attempt := 1; ; attempt++ {
		if err = db.Ping(); err == nil {
			break
		}
		if attempt == 30 {
			log.Fatalf("can't reach MariaDB at %s: %v", cfg.Addr, err)
		}
		log.Printf("waiting for MariaDB at %s: %v", cfg.Addr, err)
		time.Sleep(2 * time.Second)
	}
	createDatabase(db, dbName)
	db.Close()
	cfg.DBName = dbName
	if db, err = sql.Open("mysql", cfg.FormatDSN()); err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := migrate(db); err != nil {
		log.Fatalf("creating tables: %v", err)
	}

	web := NewWebAPI(db)
	api := &API{db: db, onSync: web.SyncFinished}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", api.health)
	mux.Handle("POST /v1/sessions", requireToken(token, api.start))
	mux.Handle("POST /v1/sessions/{id}/items", requireToken(token, api.items))
	mux.Handle("POST /v1/sessions/{id}/finish", requireToken(token, api.finish))
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, errors.New("not found"))
	})
	webPassword := os.Getenv("WEB_PASSWORD")
	if webPassword == "" {
		log.Printf("WEB_PASSWORD is not set: the web app is open to anyone who can reach this port")
	}
	registerWeb(mux, web, newWebAuth(webPassword, token))
	web.Backfill()

	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":47800"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	cert, key := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if cert != "" && key != "" {
		log.Printf("listening on %s (HTTPS), writing to %s/%s", srv.Addr, cfg.Addr, dbName)
		log.Fatal(srv.ListenAndServeTLS(cert, key))
	}
	log.Printf("listening on %s (HTTP), writing to %s/%s", srv.Addr, cfg.Addr, dbName)
	log.Fatal(srv.ListenAndServe())
}

func healthcheck() int {
	addr := env("LISTEN_ADDR", ":47800")
	scheme := "http"
	client := &http.Client{Timeout: 5 * time.Second}
	if os.Getenv("TLS_CERT_FILE") != "" {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // our own cert, on localhost
	}
	resp, err := client.Get(scheme + "://localhost" + addr[strings.LastIndex(addr, ":"):] + "/v1/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

type API struct {
	db     *sql.DB
	onSync func() // after a sync finishes
}

// GET /v1/health, unauthenticated, for the proxy and the container healthcheck.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	v, err := schemaVersion(a.db)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "schemaVersion": v})
}

// POST /v1/sessions with {"deviceName","systemVersion","fullResync"} starts a sync.
func (a *API) start(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var h Hello
	if err := json.Unmarshal(body, &h); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, err := StartSync(a.db, h)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("session %d: started by %s (iOS %s), full resync: %v, from %s", id, h.DeviceName, h.SystemVersion, h.FullResync, clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]int64{"sessionId": id})
}

// POST /v1/sessions/{id}/items: NDJSON lines (optionally gzip-compressed) and an X-Batch-Seq header. The batch is
// applied in one transaction together with its sequence number, so a batch resent after a lost reply is acknowledged
// without being applied twice.
func (a *API) items(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	seq, err := strconv.Atoi(r.Header.Get("X-Batch-Seq"))
	if err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, errors.New("X-Batch-Seq header must be a positive integer"))
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	start := time.Now()
	rollupMu.Lock()
	defer rollupMu.Unlock()
	s := NewSession(a.db)
	fresh, err := s.ClaimBatch(id, seq)
	if errors.Is(err, sql.ErrNoRows) {
		s.Rollback()
		writeError(w, http.StatusNotFound, fmt.Errorf("no session %d", id))
		return
	}
	if err != nil {
		s.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !fresh {
		s.Rollback()
		log.Printf("session %d: batch %d already applied, skipped", id, seq)
		writeJSON(w, http.StatusOK, map[string]any{"applied": 0, "duplicate": true})
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), maxBody)
	lines := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines++
		msg, err := decodeMessage(line)
		if err != nil {
			s.Rollback()
			writeError(w, http.StatusBadRequest, fmt.Errorf("line %d: %v", lines, err))
			return
		}
		if err := s.Apply(msg); err != nil {
			s.Rollback()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := sc.Err(); err != nil {
		s.Rollback()
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Commit(); err != nil {
		s.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("session %d: batch %d, %d lines, %d items applied in %s", id, seq, lines, s.Applied, time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusOK, map[string]int{"applied": s.Applied})
}

// POST /v1/sessions/{id}/finish with {"totalSent": N} closes the sync and returns the number of rows stored.
func (a *API) finish(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var d Done
	if err := json.Unmarshal(body, &d); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := FinishSync(a.db, id, d.TotalSent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := TotalRows(a.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if a.onSync != nil {
		a.onSync()
	}
	log.Printf("session %d: finished, %d items sent; %d rows in the database", id, d.TotalSent, total)
	writeJSON(w, http.StatusOK, map[string]int{"received": total})
}

func sessionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("unknown session"))
		return 0, false
	}
	return id, true
}

func requireToken(token string, next http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			log.Printf("%s %s %s: rejected, wrong or missing token", clientIP(r), r.Method, r.URL.Path)
			writeError(w, http.StatusUnauthorized, errors.New("missing or wrong token"))
			return
		}
		next(w, r)
	})
}

var errTooLarge = errors.New("request body too large")

// readBody reads the whole body, undoing Content-Encoding gzip or deflate, capped at maxBody.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		src = zr
	case "deflate":
		// Officially zlib-wrapped, but raw deflate is common; accept both.
		raw, err := io.ReadAll(src)
		if err != nil {
			return nil, err
		}
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer zr.Close()
			src = zr
		} else {
			src = flate.NewReader(bytes.NewReader(raw))
		}
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", r.Header.Get("Content-Encoding"))
	}
	body, err := io.ReadAll(io.LimitReader(src, maxBody+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errTooLarge
		}
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errTooLarge
	}
	return body, nil
}

func writeBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

// clientIP prefers the address a reverse proxy reports.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		log.Printf("error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package main

// The fitness web app: static files embedded in the binary, a read-only JSON API under /api, and an optional
// password login. The upload API under /v1 is unchanged and keeps its own bearer token.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

const sessionCookie = "iohealth_session"

type webAuth struct {
	password string // empty: no login
	syncKey  string // SYNC_TOKEN also opens the API, for scripts
	secret   []byte
	lifetime time.Duration
}

func newWebAuth(password, syncToken string) *webAuth {
	// Derived from both secrets, so changing either one signs everybody out.
	m := hmac.New(sha256.New, []byte(syncToken))
	m.Write([]byte("web-session\x00" + password))
	return &webAuth{password: password, syncKey: syncToken, secret: m.Sum(nil), lifetime: 30 * 24 * time.Hour}
}

func (a *webAuth) sign(expires int64) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(strconv.FormatInt(expires, 10)))
	return strconv.FormatInt(expires, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (a *webAuth) valid(r *http.Request) bool {
	if a.password == "" {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.syncKey)) == 1 {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.sign(n)))
}

// loginLimiter slows down password guessing: a few attempts per minute per client address.
type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var keep []time.Time
	for _, t := range l.hits[ip] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		l.hits[ip] = keep
		return false
	}
	l.hits[ip] = append(keep, now)
	return true
}

func (a *webAuth) login(w http.ResponseWriter, r *http.Request, lim *loginLimiter) {
	if !lim.allow(clientIP(r)) {
		http.Error(w, "too many attempts, wait a minute", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	pw := r.FormValue("password")
	if a.password == "" || subtle.ConstantTimeCompare([]byte(pw), []byte(a.password)) != 1 {
		log.Printf("%s: web login failed", clientIP(r))
		time.Sleep(500 * time.Millisecond)
		http.Redirect(w, r, "login.html?failed=1", http.StatusSeeOther)
		return
	}
	exp := time.Now().Add(a.lifetime).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: a.sign(exp), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", Expires: time.Unix(exp, 0),
	})
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

func (a *webAuth) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "login.html", http.StatusSeeOther)
}

// registerWeb adds the web app and its API to mux.
func registerWeb(mux *http.ServeMux, w *WebAPI, auth *webAuth) {
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	files := http.FileServer(http.FS(static))
	lim := &loginLimiter{hits: map[string][]time.Time{}}

	mux.HandleFunc("POST /login", func(rw http.ResponseWriter, r *http.Request) { auth.login(rw, r, lim) })
	mux.HandleFunc("GET /logout", auth.logout)

	api := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(rw http.ResponseWriter, r *http.Request) {
			if !auth.valid(r) {
				writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
				return
			}
			rw.Header().Set("Cache-Control", "no-store")
			h(rw, r)
		})
	}
	api("GET /api/status", w.status)
	api("GET /api/types", w.types)
	api("GET /api/series", w.series)
	api("GET /api/workouts", w.workouts)
	api("GET /api/workouts/{uuid}", w.workout)
	api("GET /api/routes", w.routes)
	api("GET /api/sleep", w.sleep)
	api("GET /api/activity", w.activity)
	api("GET /api/profile", w.profile)
	api("GET /api/records", w.records)
	api("GET /api/records/efforts", w.efforts)
	api("GET /api/glucose", w.glucose)
	api("GET /api/settings", w.getSettings)
	api("PUT /api/settings", w.putSettings)
	mux.HandleFunc("/api/", func(rw http.ResponseWriter, r *http.Request) {
		writeError(rw, http.StatusNotFound, errNotFound)
	})

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(rw, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		p := r.URL.Path
		// The login page and what it needs, plus the home-screen manifest and icons, which phones fetch without the
		// session cookie.
		public := p == "/login.html" || strings.HasPrefix(p, "/vendor/") || p == "/app.css" || p == "/favicon.svg" ||
			p == "/manifest.webmanifest" || p == "/apple-touch-icon.png" || strings.HasPrefix(p, "/icon-")
		if !public && !auth.valid(r) {
			http.Redirect(rw, r, "login.html", http.StatusSeeOther)
			return
		}
		if p == "/" || p == "/index.html" || p == "/login.html" {
			rw.Header().Set("Cache-Control", "no-cache")
		}
		if p == "/manifest.webmanifest" {
			rw.Header().Set("Content-Type", "application/manifest+json")
		}
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Referrer-Policy", "same-origin")
		files.ServeHTTP(rw, r)
	})
}

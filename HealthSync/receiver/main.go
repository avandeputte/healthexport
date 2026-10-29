// Command healthsync-receiver accepts Health Sync uploads from the iPhone app over TCP (newline-delimited JSON,
// see Shared/SyncProtocol.swift) and writes them to MariaDB.
package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	if os.Getenv("DB_HOST") == "" {
		log.Fatal("DB_HOST is not set; see .env.example")
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
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		log.Fatalf("creating tables: %v", err)
	}

	addr := env("LISTEN_ADDR", ":47800")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s, writing to %s/%s", addr, cfg.Addr, dbName)

	var busy sync.Mutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go func() {
			// One sync at a time; a second phone is turned away rather than interleaving writes.
			if !busy.TryLock() {
				log.Printf("%s: refused, another sync is running", conn.RemoteAddr())
				conn.Close()
				return
			}
			defer busy.Unlock()
			handle(db, conn)
		}()
	}
}

func handle(db *sql.DB, conn net.Conn) {
	defer conn.Close()
	peer := conn.RemoteAddr().String()
	log.Printf("%s: connected", peer)
	start := time.Now()
	s := NewSession(db)
	finished := false
	defer func() {
		if finished {
			return
		}
		// Keep whatever arrived; every insert is an upsert, so the next sync fills the gap.
		if err := s.Commit(); err != nil {
			log.Printf("%s: saving partial sync: %v", peer, err)
			s.Rollback()
		}
		log.Printf("%s: disconnected before finishing, kept %d items", peer, s.Applied)
	}()

	r := bufio.NewReaderSize(conn, 1<<20)
	nextLog := 100_000
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Lines longer than the buffer (e.g. a big workout): gather the rest.
			full := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				full = append(full, line...)
			}
			line = full
		}
		if err != nil {
			if len(line) > 0 {
				log.Printf("%s: %v", peer, err)
			}
			return
		}
		if len(line) <= 1 {
			continue
		}
		msg, err := decodeMessage(line)
		if err != nil {
			log.Printf("%s: bad line: %v", peer, err)
			return
		}
		if msg.Hello != nil {
			log.Printf("%s: %s (iOS %s), full resync: %v", peer, msg.Hello.DeviceName, msg.Hello.SystemVersion, msg.Hello.FullResync)
		}
		done, err := s.Apply(msg)
		if err != nil {
			log.Printf("%s: %v", peer, err)
			s.Rollback()
			finished = true
			return
		}
		if s.Applied >= nextLog {
			log.Printf("%s: %d items", peer, s.Applied)
			nextLog += 100_000
		}
		if done {
			finished = true
			total, err := TotalRows(db)
			if err != nil {
				log.Printf("%s: counting rows: %v", peer, err)
			}
			fmt.Fprintf(conn, "{\"ack\":{\"received\":%d}}\n", total)
			log.Printf("%s: sync finished, %d items in %s; %d rows in the database", peer, s.Applied, time.Since(start).Round(time.Second), total)
			return
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

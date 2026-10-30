# Health Sync

Copies your Apple Health data, all of it or just the types you pick, into your own MariaDB database. Nothing goes
through a cloud service: an iPhone app uploads straight to a small receiver that you run in Docker, over HTTPS behind
your reverse proxy (or plain HTTP on your own network).

Health data only exists on the iPhone (on macOS, `HKHealthStore.isHealthDataAvailable()` is always `false`), which is
why there are two parts:

```
┌──────────────┐   HTTPS (REST, gzip NDJSON)   ┌───────────────┐        ┌──────────┐
│  Health Sync │ ────────────────────────────▶ │ reverse proxy │ ─────▶ │ receiver │ ──▶ MariaDB
│  (iPhone)    │                               │ (TLS)         │  HTTP  │ (Docker) │
└──────────────┘                               └───────────────┘        └──────────┘
```

| path | what |
|---|---|
| `HealthSync/receiver/` | Go service, shipped as a Docker image, that receives the uploads and writes MariaDB |
| `HealthSync/iOS/` | the iPhone app |
| `HealthSync/Shared/` | the upload line format (`SyncProtocol.swift`) shared by the apps |
| `HealthSync/Mac/`, `HealthSync/ExportImporter/` | legacy macOS tools (a SQLite receiver the phone no longer talks to, and an importer for the Health app's `export.zip`) |
| `HealthSync/project.yml` | [XcodeGen](https://github.com/yonaskolb/XcodeGen) spec that generates the Xcode project |
| `.github/workflows/receiver-image.yml` | builds and publishes the receiver image |

## 1. Run the receiver (Docker)

The receiver is published as `ghcr.io/avandeputte/healthexport-receiver` (public, `linux/amd64` and `linux/arm64`).
Tags: `latest` (current `main`), `1.0.0` and other release versions, and one tag per branch while it's being worked on.
It needs an existing MariaDB (or MySQL) server; it creates the database if the user may, and creates and migrates its
own tables.

### Configuration

All settings are environment variables, set in the compose file, with `-e` flags, or from a `.env` file; never commit
real values.

| variable | required | meaning |
|---|---|---|
| `DB_HOST` | yes | MariaDB host name or IP |
| `DB_PORT` | no | default `3306` |
| `DB_USER`, `DB_PASSWORD` | yes | a user that may create tables in `DB_NAME` (and the database itself, ideally) |
| `DB_NAME` | no | default `health` |
| `SYNC_TOKEN` | yes | the bearer token the phone must send; make one with `openssl rand -hex 32` |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | no | set both (and mount the files) to serve HTTPS directly instead of behind a proxy |
| `LISTEN_ADDR` | no | address the receiver listens on inside the container, default `:47800` |

The container listens on port **47800**.

### With Docker Compose (recommended)

Save this as `docker-compose.yml`, fill in the values, and run `docker compose up -d`. Every option the receiver
understands is listed (the same file is in the repo as `HealthSync/receiver/docker-compose.ghcr.yml`):

```yaml
services:
  receiver:
    image: ghcr.io/avandeputte/healthexport-receiver:latest   # or a version such as 1.0.0
    container_name: health-receiver
    restart: unless-stopped
    ports:
      - "47800:47800"                 # host:container; change the host side if 47800 is taken
    environment:
      # --- MariaDB / MySQL (required) ---
      DB_HOST: 192.168.1.10           # host name or IP of the database server
      DB_PORT: "3306"
      DB_USER: health
      DB_PASSWORD: change-me
      DB_NAME: health                 # created on first start if the user may; tables are always created/migrated
      # --- Phone authentication (required) ---
      SYNC_TOKEN: change-me-to-a-long-random-string   # at least 16 characters; openssl rand -hex 32
      # --- Optional ---
      LISTEN_ADDR: ":47800"           # address:port inside the container
      # Serve HTTPS directly instead of behind a reverse proxy: set both and mount the files below.
      # TLS_CERT_FILE: /certs/fullchain.pem
      # TLS_KEY_FILE: /certs/privkey.pem
    # volumes:
    #   - /path/to/certs:/certs:ro
    healthcheck:
      test: ["CMD", "/receiver", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
```

```sh
docker compose up -d
curl http://localhost:47800/v1/health        # {"ok":true,"schemaVersion":5}
```

Update later with `docker compose pull && docker compose up -d`.

<details>
<summary>Keeping the secrets in a <code>.env</code> file instead</summary>

If you'd rather not have the password and token inside the compose file, replace the `environment:` block with
`env_file: .env` and put the settings in a `.env` next to it (`HealthSync/receiver/.env.example` is a template):

```yaml
services:
  receiver:
    image: ghcr.io/avandeputte/healthexport-receiver:latest
    container_name: health-receiver
    restart: unless-stopped
    ports:
      - "47800:47800"
    env_file: .env
    healthcheck:
      test: ["CMD", "/receiver", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
```

```dotenv
# .env
DB_HOST=192.168.1.10
DB_PORT=3306
DB_USER=health
DB_PASSWORD=change-me
DB_NAME=health
SYNC_TOKEN=change-me-to-a-long-random-string
# LISTEN_ADDR=:47800
# TLS_CERT_FILE=/certs/fullchain.pem
# TLS_KEY_FILE=/certs/privkey.pem
```
</details>

### With plain `docker run`

```sh
docker run -d --name health-receiver --restart unless-stopped -p 47800:47800 \
  -e DB_HOST=192.168.1.10 -e DB_USER=health -e DB_PASSWORD=change-me -e DB_NAME=health \
  -e SYNC_TOKEN=change-me-to-a-long-random-string \
  ghcr.io/avandeputte/healthexport-receiver:latest
docker logs -f health-receiver
```

(or `--env-file .env` instead of the `-e` flags, with the `.env` shown above.)

### Building it yourself

From a clone, `cd HealthSync/receiver && docker compose up -d` builds the image locally instead of pulling it
(`docker-compose.yml` has `build: .`). `go test ./...` runs the receiver's tests.

### HTTPS

The container speaks plain HTTP; put it behind your reverse proxy for HTTPS. With Caddy, which gets a certificate by
itself:

```
health.example.com {
    reverse_proxy 192.168.1.10:47800
}
```

With nginx:

```nginx
server {
    listen 443 ssl;
    server_name health.example.com;
    # ssl_certificate / ssl_certificate_key …
    client_max_body_size 100m;     # batches are gzip-compressed and normally well under 1 MB
    proxy_read_timeout 300s;
    location / {
        proxy_pass http://192.168.1.10:47800;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

Traefik: route the host to port 47800 and raise the body limit if you use the buffering middleware. If the proxy runs
in the same Docker network, use `receiver:47800` as the upstream. The phone allows plain `http://` only to local
network addresses; anything else must be `https://`.

## 2. Build and install the iPhone app

You need a Mac with Xcode 15 or later, an iPhone on iOS 17 or later, and an Apple developer team (a free Apple ID
works, but the app then has to be re-installed every 7 days).

```sh
brew install xcodegen        # once
git clone https://github.com/avandeputte/healthexport && cd healthexport/HealthSync
xcodegen generate
open HealthSync.xcodeproj
```

In Xcode select the **HealthSync** target → *Signing & Capabilities* → pick your Team (HealthKit and background
delivery are already enabled through the entitlements in `project.yml`), plug in your iPhone, choose it as the run
destination, and run.

## 3. Use it

1. On the iPhone, open **Health Sync**, tap **Allow access to Health data**, and on Apple's permission screen turn
   everything on. (What you actually sync is chosen separately in step 3, without asking again.)
2. Under **Server**, enter the receiver's URL (`https://health.example.com`, or `http://192.168.1.10:47800` at home)
   and the token (`SYNC_TOKEN` from the receiver's `.env`; it's kept in the iPhone's Keychain). **Test connection**
   checks the address.
3. Optionally open **Data types** and switch off what you don't want: types are grouped, searchable, and marked when
   the phone has no data for them.
4. Tap **Sync now**. A decade of Apple Watch data (a few million samples) takes a few minutes; keep the app open for
   the first sync.

Later syncs only send what's new or changed. Turn on **Send everything again** when the database is new or emptied.

**Automatic syncing.** With "Sync automatically" on (the default), the phone syncs as soon as the app is opened, and
iOS also wakes it in the background when new Health data arrives (HealthKit background delivery, roughly hourly, only
while the phone is unlocked) plus an occasional background-refresh slot. The receiver has to be reachable at that
moment; a missed or cut-short round costs nothing, because anchors only advance for batches the server has
acknowledged.

## What you get

All tables are InnoDB/utf8mb4 with timestamps in UTC as `DATETIME(3)`:

| table | contents |
|---|---|
| `samples` | every quantity and category sample: `type` (a short snake_case name: `heart_rate`, `step_count`, `sleep_analysis`, `vo2_max`…), `kind` (`quantity`/`category`/…), `value`, `unit`, `start_date`, `end_date`, `count` (more than 1 for samples some apps pre-aggregate), `text` (names for category values such as `asleep`, `deep`, `rem`, or audiogram points as JSON), `source_name` (the app or device that recorded it), `metadata_json` |
| `workouts`, `workout_statistics`, `workout_events`, `workout_activities` | workouts and their per-workout heart rate / distance / energy stats, laps and pauses, and multisport segments |
| `workout_route_points` | GPS traces (lat, lon, altitude, speed, course, accuracy) |
| `heartbeats` | beat-to-beat intervals behind HRV readings |
| `activity_summaries` | daily Move / Exercise / Stand rings and goals |
| `characteristics` | date of birth, biological sex, blood type, skin type, wheelchair use |
| `deleted_objects`, `sync_log`, `schema_version` | bookkeeping |

Every data table also has `inserted_at` (when the receiver first stored the row) and `updated_at` (when its values last
changed; equal to `inserted_at` until then), both UTC. Resending unchanged data touches neither. The lap, pause and
multisport rows of a workout are rewritten whenever the workout is resent, so theirs restart then. Rows written
before these columns existed (receiver schema versions below 5) have `NULL`.

Type names are the HealthKit identifier without its prefix, in snake_case (`HKQuantityTypeIdentifierHeartRateVariabilitySDNN`
→ `heart_rate_variability_sdnn`); the phone and the receiver share the rule (`HealthTypes.storageName`,
`receiver/typenames.go`). Units are your Health app's preferred units (kg or lb, km or mi…) where Apple exposes them,
otherwise metric; the `unit` column always says which.

```sql
-- resting heart rate, last 30 days
SELECT DATE(start_date) d, ROUND(AVG(value)) bpm
FROM samples WHERE type = 'resting_heart_rate'
GROUP BY d ORDER BY d DESC LIMIT 30;

-- how long after a measurement it reached the database, per type, last 7 days
SELECT type, ROUND(AVG(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60) avg_lag_min,
       ROUND(MAX(TIMESTAMPDIFF(SECOND, end_date, inserted_at)) / 60) max_lag_min, COUNT(*) n
FROM samples WHERE inserted_at > UTC_TIMESTAMP() - INTERVAL 7 DAY
GROUP BY type ORDER BY avg_lag_min DESC;

-- hours of sleep per night by stage
SELECT DATE(start_date - INTERVAL 12 HOUR) night, text stage,
       ROUND(SUM(TIMESTAMPDIFF(SECOND, start_date, end_date)) / 3600, 2) hours
FROM samples WHERE type = 'sleep_analysis'
GROUP BY night, stage ORDER BY night DESC;
```

## API

Every `/v1` call except `/v1/health` needs `Authorization: Bearer <SYNC_TOKEN>`; errors are JSON `{"error": "…"}`
with 400, 401, 404 or 413.

| request | body | reply |
|---|---|---|
| `GET /v1/health` | | `{"ok":true,"schemaVersion":N}` |
| `POST /v1/sessions` | `{"deviceName","systemVersion","fullResync"}` | 201 `{"sessionId":N}` |
| `POST /v1/sessions/{id}/items` | NDJSON lines as in `HealthSync/Shared/SyncProtocol.swift`, optionally `Content-Encoding: gzip`; header `X-Batch-Seq: n` | `{"applied":k}` |
| `POST /v1/sessions/{id}/finish` | `{"totalSent":N}` | `{"received":rows}` |

Each batch is committed in one transaction together with its sequence number, so a batch resent after a lost reply is
acknowledged (`"duplicate":true`) without being applied twice. Every write is an upsert keyed on HealthKit's UUIDs, so
resending anything is safe.

## Not included

- **ECGs**: the iPhone app doesn't request or send them (the voltage traces are most of the data by volume).
- **Vision prescriptions**: HealthKit only allows reading them one prescription at a time, with a separate prompt.
- **Clinical (FHIR) health records** need the separate "Clinical Health Records" entitlement, which Apple grants
  per app.
- Data the Watch has purged (very old raw samples that the phone never received).

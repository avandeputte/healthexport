# Health Sync

Copies your Apple Health data, all of it or the types you pick, into your own MariaDB database. Nothing goes through
a cloud service: the iPhone app uploads straight to a small receiver you run in Docker, over HTTPS behind your
reverse proxy or plain HTTP on your own network.

Why an iPhone app: Health data only lives on the iPhone (on macOS, `HKHealthStore.isHealthDataAvailable()` is always
`false`), so a small app reads it there and sends it to the receiver, which writes the database.

```
┌──────────────┐   HTTPS (REST, gzip NDJSON)   ┌───────────────┐        ┌─────────┐
│  HealthSync  │ ────────────────────────────▶ │ reverse proxy │ ─────▶ │receiver │ ──▶ MariaDB
│  (iPhone)    │                               │ (TLS)         │  HTTP  │(Docker) │
└──────────────┘                               └───────────────┘        └─────────┘
```

| folder | what |
|---|---|
| `iOS/` | iPhone app: asks for Health permission, reads the selected types with anchored queries (so later syncs only send changes), uploads them in batches |
| `receiver/` | Docker service with an HTTP API that receives the uploads and writes MariaDB |
| `Shared/` | the line format (`SyncProtocol.swift`) and a SQLite wrapper |
| `Mac/` | legacy Mac app that received over raw TCP into SQLite; it still builds but the phone no longer talks to it |
| `ExportImporter/` | optional Mac app that imports the Health app's `export.zip` into SQLite (handy for a one‑off) |
| `project.yml` | [XcodeGen](https://github.com/yonaskolb/XcodeGen) spec that generates the Xcode project |

## Build the iPhone app

You need Xcode 15 or later and an iPhone on iOS 17 or later.

```sh
brew install xcodegen        # once
cd HealthSync
xcodegen generate
open HealthSync.xcodeproj
```

Select the **HealthSync** target → *Signing & Capabilities* → pick your Team (a free Apple ID works, but the app
then has to be re-installed every 7 days). HealthKit is already enabled through the entitlements in `project.yml`.
Plug in your iPhone, choose it as the run destination, and run **HealthSync**.

## Run the receiver

```sh
cd receiver
cp .env.example .env      # DB_HOST, DB_USER, DB_PASSWORD, DB_NAME, and SYNC_TOKEN=$(openssl rand -hex 32)
docker compose up -d
curl http://localhost:47800/v1/health      # {"ok":true,"schemaVersion":2}
```

It creates the database (if the user may) and its tables on first start; later schema changes are numbered
migrations recorded in `schema_version` and applied automatically.

### HTTPS

The container speaks plain HTTP on port 47800; put it behind your reverse proxy for HTTPS. Caddy, which gets a
certificate by itself:

```
health.example.com {
    reverse_proxy 192.168.1.168:47800
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name health.example.com;
    # ssl_certificate / ssl_certificate_key …
    client_max_body_size 100m;     # batches are gzip-compressed and normally well under 1 MB
    proxy_read_timeout 300s;
    location / {
        proxy_pass http://192.168.1.168:47800;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

Traefik: route the host to port 47800 and raise the body limit if you use the buffering middleware. If the proxy runs
in the same Docker network, use `receiver:47800` as the upstream. Without a proxy, set `TLS_CERT_FILE` and
`TLS_KEY_FILE` (and mount the files) to make the receiver serve HTTPS itself. The phone allows plain `http://` only to
local addresses; anything else must be `https://`.

## Use

1. On the iPhone, open **Health Sync**, tap **Allow access to Health data**, and on Apple's permission screen
   turn everything on. (What you actually sync is chosen separately, below, without asking again.)
2. Under **Server**, enter the receiver's URL (`https://health.example.com`, or `http://192.168.1.168:47800` at
   home) and the token (`SYNC_TOKEN` from the receiver's `.env`; it's kept in the iPhone's Keychain). **Test
   connection** checks the address.
3. Optionally open **Data types** and switch off what you don't want: types are grouped, searchable, and marked when
   the phone has no data for them.
4. Tap **Sync now**. A decade of Apple Watch data (several million samples) takes a few minutes; keep the app open
   for the first sync.

Later syncs only send what's new or changed. Turn on **Send everything again** when the database is new or emptied.

**Automatic syncing.** With "Sync automatically" on (the default), the phone syncs as soon as the app is opened, and
iOS also wakes it in the background when new Health data arrives (HealthKit background delivery, roughly hourly, only
while the phone is unlocked) plus an occasional background-refresh slot. A missed or cut-short round costs nothing:
anchors only advance for batches the server has acknowledged.

## API

Every `/v1` call except `/v1/health` needs `Authorization: Bearer <SYNC_TOKEN>`; errors are JSON `{"error": "…"}` with
401, 404, 413 or 400.

| request | body | reply |
|---|---|---|
| `GET /v1/health` | | `{"ok":true,"schemaVersion":N}` |
| `POST /v1/sessions` | `{"deviceName","systemVersion","fullResync"}` | 201 `{"sessionId":N}` |
| `POST /v1/sessions/{id}/items` | NDJSON lines as in `Shared/SyncProtocol.swift`, optionally `Content-Encoding: gzip`; header `X-Batch-Seq: n` | `{"applied":k}` |
| `POST /v1/sessions/{id}/finish` | `{"totalSent":N}` | `{"received":rows}` |

Each batch is committed in one transaction together with its sequence number, so a batch resent after a lost reply is
acknowledged (`"duplicate":true`) without being applied twice. Every write is an upsert keyed on HealthKit's UUIDs, so
resending anything is safe.

## What you get

All InnoDB/utf8mb4, timestamps in UTC as `DATETIME(3)`:

| table | contents |
|---|---|
| `samples` | every quantity and category sample: `type` (a short snake_case name: `heart_rate`, `step_count`, `sleep_analysis`, `vo2_max`…), `value`, `unit`, `start_date`, `end_date`, `count` (more than 1 for samples some apps pre-aggregate); `text` holds names for category values (`asleep`, `deep`, `rem`…), audiogram points as JSON, etc.; `source_name` is the app or device that recorded it; `metadata_json` carries the rest |
| `workouts`, `workout_statistics`, `workout_events`, `workout_activities` | workouts (with `source_name` and `metadata_json`) and their per‑workout heart rate / distance / energy stats, laps/pauses, and multisport segments |
| `workout_route_points` | GPS traces (lat, lon, altitude, speed, course, accuracy) |
| `heartbeats` | beat‑to‑beat intervals behind HRV readings |
| `activity_summaries` | daily Move / Exercise / Stand rings and goals |
| `characteristics` | date of birth, biological sex, blood type, skin type, wheelchair use |
| `deleted_objects`, `sync_log`, `schema_version` | bookkeeping |

Type names are the HealthKit identifier without its prefix, in snake_case (`HKQuantityTypeIdentifierHeartRateVariabilitySDNN`
→ `heart_rate_variability_sdnn`); the phone and the receiver share the rule (`HealthTypes.storageName`,
`receiver/typenames.go`).

Units are your Health app's preferred units (kg or lb, km or mi …) where Apple exposes them, otherwise metric;
the `unit` column always says which.

Example queries:

```sql
-- resting heart rate, last 30 days
SELECT DATE(start_date) d, ROUND(AVG(value)) bpm
FROM samples WHERE type = 'resting_heart_rate'
GROUP BY d ORDER BY d DESC LIMIT 30;

-- hours of sleep per night by stage
SELECT DATE(start_date - INTERVAL 12 HOUR) night, text stage,
       ROUND(SUM(TIMESTAMPDIFF(SECOND, start_date, end_date)) / 3600, 2) hours
FROM samples WHERE type = 'sleep_analysis'
GROUP BY night, stage ORDER BY night DESC;
```

## Not included

- **ECGs**: the iPhone app doesn't request or send them (the voltage traces are most of the data and Alex has no use
  for them).
- **Vision prescriptions**: HealthKit only allows reading them one prescription at a time, with a separate prompt.
- **Clinical (FHIR) health records** need the separate "Clinical Health Records" entitlement, which Apple grants
  per app.
- Data the Watch has purged (very old raw samples that the phone never received).

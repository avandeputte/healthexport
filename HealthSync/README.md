# Health Sync

Copies **all** of your Apple Health data to your Mac, into a SQLite database (and optionally CSV files), over your
own Wi‑Fi. Nothing goes through a cloud service.

Why two apps: macOS has no Health database of its own. Apple's HealthKit docs say the framework compiles on
macOS 13+ but `isHealthDataAvailable()` is always `false` there. The data lives on the iPhone, so a small iPhone
app reads it and streams it to a Mac app that writes the database.

```
┌──────────────┐  Bonjour + TCP (NDJSON)  ┌──────────────────┐
│  HealthSync  │ ───────────────────────▶ │  Health Sync.app │ ──▶ health.sqlite
│  (iPhone)    │                          │  (Mac)           │ ──▶ health.csv/
└──────────────┘                          └──────────────────┘
```

| folder | what |
|---|---|
| `iOS/` | iPhone app: asks for Health permission, reads every type with anchored queries (so later syncs only send changes), finds the Mac with Bonjour, streams the data |
| `Mac/` | Mac app: advertises itself, receives the stream, writes SQLite, exports CSV |
| `Shared/` | the wire format (`SyncProtocol.swift`) and the SQLite wrapper |
| `ExportImporter/` | a third, optional Mac app that imports the Health app's `export.zip` instead (handy for a one‑off, or a phone you can't install the companion on) |
| `project.yml` | [XcodeGen](https://github.com/yonaskolb/XcodeGen) spec that generates the Xcode project with all three targets |

## Build

You need Xcode 15 or later, an iPhone on iOS 17 or later, and a Mac on macOS 14 or later.

```sh
brew install xcodegen        # once
cd HealthSync
xcodegen generate
open HealthSync.xcodeproj
```

In Xcode:

1. Select the **HealthSync** target → *Signing & Capabilities* → pick your Team. A free Apple ID works
   (the app then has to be re-installed every 7 days; a paid developer account removes that limit).
   HealthKit is already enabled through `iOS/HealthSync.entitlements`.
2. Do the same for **HealthSyncMac** (and **HealthExportImporter** if you want it).
3. Plug in your iPhone, choose it as the run destination, and run **HealthSync**.
4. Choose *My Mac* as the destination and run **HealthSyncMac**.

<details>
<summary>Without XcodeGen</summary>

Create a new Xcode project (Multiplatform → App, or one iOS App and one macOS App), delete the generated Swift files,
and drag in the folders above: the iOS target gets `iOS/` + `Shared/SyncProtocol.swift`, the Mac target gets
`Mac/` + `Shared/`. Add the HealthKit capability to the iOS target, "Incoming Connections (Server)" and
"Outgoing Connections (Client)" to the Mac target's App Sandbox, link `libsqlite3.tbd` to the Mac target, and copy
the `Info.plist` keys listed under `info.properties` in `project.yml` into each target's Info tab.
</details>

## Use

1. On the Mac, open **Health Sync** and click **New…** to choose where `health.sqlite` lives.
   It then shows "Waiting for your iPhone".
2. On the iPhone, open **Health Sync**, tap **Allow access to Health data**, and on Apple's permission screen
   tap **Turn On All**. (Anything you leave off is simply not exported.)
3. Your Mac appears under "Macs on this network". Tap it. Both devices need to be on the same Wi‑Fi;
   the first time, iOS and macOS each ask once for local network permission.
4. Wait. A decade of Apple Watch data (several million heart‑rate samples) takes a few minutes; keep the
   iPhone app in the foreground. Both apps show progress.

Running it again later sends only what's new or changed. If you create a fresh database on the Mac, turn on
**Send everything again** on the phone first.

## What you get

`health.sqlite`:

| table | contents |
|---|---|
| `samples` | every quantity and category sample: `type` (e.g. `HKQuantityTypeIdentifierHeartRate`), `value`, `unit`, `start_date`, `end_date`; `text` holds names for category values (`asleep`, `deep`, `rem`…), audiogram points as JSON, etc.; `metadata_json`, `device_json` and `source_*` carry the rest |
| `workouts`, `workout_statistics`, `workout_events`, `workout_activities` | workouts with per‑workout heart rate / distance / energy stats, laps/pauses, and multisport segments |
| `workout_route_points` | GPS traces (lat, lon, altitude, speed, course, accuracy) |
| `ecgs`, `ecg_voltages` | every ECG, with the full 512 Hz voltage trace |
| `heartbeats` | beat‑to‑beat intervals behind HRV readings |
| `activity_summaries` | daily Move / Exercise / Stand rings and goals |
| `characteristics` | date of birth, biological sex, blood type, skin type, wheelchair use |
| `deleted_objects`, `sync_log` | bookkeeping |

Units are your Health app's preferred units (kg or lb, km or mi …) where Apple exposes them, otherwise metric;
the `unit` column always says which.

"Export CSV now" writes `health.csv/` next to the database with one file per type (`HeartRate.csv`,
`StepCount.csv`, `SleepAnalysis.csv`, …) plus one per table above.

Example queries:

```sql
-- resting heart rate, last 30 days
SELECT date(start_date) d, round(avg(value)) bpm
FROM samples WHERE type = 'HKQuantityTypeIdentifierRestingHeartRate'
GROUP BY d ORDER BY d DESC LIMIT 30;

-- hours of sleep per night by stage
SELECT date(start_date, '-12 hours') night, text stage,
       round(sum((julianday(end_date) - julianday(start_date)) * 24), 2) hours
FROM samples WHERE type = 'HKCategoryTypeIdentifierSleepAnalysis'
GROUP BY night, stage ORDER BY night DESC;
```

## Not included

- **Clinical (FHIR) health records** need the separate "Clinical Health Records" entitlement, which Apple grants
  per app. If you use that feature, add `health-records` to `com.apple.developer.healthkit.access` in
  `iOS/HealthSync.entitlements` and open an issue; the reader can be extended.
- Data the Watch has purged (very old raw samples that the phone never received).
- Background sync: the phone app has to be open. The protocol is incremental, so a sync is quick after the first one.

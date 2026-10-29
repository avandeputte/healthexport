# Health Sync sources

The documentation lives in the [repository README](../README.md). This folder holds:

| folder | what |
|---|---|
| `receiver/` | Go service, shipped as the Docker image `ghcr.io/avandeputte/healthexport-receiver`, that receives uploads and writes MariaDB. `docker-compose.yml` builds it locally; `docker-compose.ghcr.yml` pulls the published image; `.env.example` lists the settings |
| `iOS/` | iPhone app: Health permission, per-type selection, anchored reads, batched uploads, background sync |
| `Shared/` | the upload line format (`SyncProtocol.swift`) and a SQLite wrapper used by the legacy Mac tools |
| `Mac/` | legacy macOS receiver that wrote SQLite over raw TCP; still builds, but the phone no longer uses it |
| `ExportImporter/` | macOS tool that imports the Health app's `export.zip` into SQLite |
| `project.yml` | XcodeGen spec: `xcodegen generate` produces `HealthSync.xcodeproj` with the iOS and Mac targets |

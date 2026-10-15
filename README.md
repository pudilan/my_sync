# my_sync

Incremental, password protected, size-split backups of one or more directories
into standard zip archives, with a small web UI. Written in Go, single static
binary, deployable with Docker.

## Features

- Back up multiple source directories into zip archives in a destination directory.
- Optional archive password: **AES-256** (WinZip AE-2, opens in 7-Zip, WinRAR,
  Bandizip, Keka, …) or **ZipCrypto** (weak, but Windows Explorer can open it).
- Split by size into volumes. Every volume is a **complete, standalone zip** —
  any volume can be extracted on its own without the others.
- Incremental: a JSON database (`index.json`) records every backed up file, so
  the next run only compares against the database, never against archives.
- Each backup run writes its archives **and** its database into the same
  directory, so every directory is a self-contained record.
- No changes → no archive and no database are written.
- Optional per-run size cap (`max_backup_size`, e.g. `50GB`): changes beyond
  the cap are left for the next run, so a large backup can be split over
  several runs when disk space is short.
- Web UI: overview, run / preview / cancel backups with live progress and logs,
  backup history, per-backup change list and volume download, file index search,
  configuration editor. Optional basic auth.
- Built-in scheduler (interval or daily time), CLI, and an index-driven restore.

## Output layout

```
/backups/
├── 20261015-020000/                  # first run: full backup
│   ├── backup_20261015-020000_001.zip
│   ├── backup_20261015-020000_002.zip
│   └── index.json                    # database snapshot after this run
├── 20261016-020000/                  # later run: only new/changed files
│   ├── backup_20261016-020000_001.zip
│   └── index.json
├── index.latest.json                 # copy of the newest database
└── .my_sync.lock
```

Inside the archives, files are stored as `<source name>/<relative path>`.

## How incremental backups work

1. The newest database is loaded: the `index.json` of the newest backup
   directory, or `index.latest.json` in the destination root if that one is
   newer (e.g. because backup directories were moved elsewhere).
2. Sources are scanned. A file is
   - **added** if it is not in the index,
   - **unchanged** if size and mtime match the index (the file is not read),
   - **modified** otherwise. If only the mtime changed, the file is hashed and
     skipped when its SHA-256 equals the recorded one.
   - Files in the index that no longer exist are **deleted**.
3. Nothing added/modified/deleted → stop, nothing is written.
   With `max_backup_size` set, only the changed files that fit are selected
   (see below); the rest are not recorded and are detected again next run.
4. Otherwise new/modified files are written to volumes in `.tmp-<id>/`, the
   new index is saved, and the directory is atomically renamed to `<id>/`.
   An interrupted or cancelled run leaves no partial backup behind.

A run that only detected deletions writes a directory containing just
`index.json` (no archive), so the deletion is recorded.

If a source directory is missing or unreadable the run aborts instead of
treating its files as deleted. Files that cannot be read are logged, listed in
the index under `errors`, and retried next run.

### `index.json` format

```jsonc
{
  "format": "my_sync/index/v1",
  "backup_id": "20261016-020000",
  "previous_id": "20261015-020000",
  "started_at": "...", "finished_at": "...",
  "sources": [{"name": "docs", "path": "/data/docs"}],
  "encrypted": true, "encryption": "aes256",
  "compression": "deflate", "volume_size": 1073741824,
  "volumes": [{"name": "backup_20261016-020000_001.zip", "size": 1234, "files": 2, "sha256": "..."}],
  "stats": {"added": 1, "modified": 1, "deleted": 0, "errors": 0, "bytes": 0,
            "archive_bytes": 0, "total_files": 0, "total_bytes": 0},
  "changes": [{"path": "docs/a.txt", "op": "modified", "size": 14, "volume": "..."}],
  // complete state of all tracked files; "backup"/"volume" point to the
  // archive that holds the latest version of each file
  "files": {
    "docs/a.txt": {"size": 14, "mtime": 1792050416496683712, "sha256": "...",
                   "backup": "20261016-020000", "volume": "backup_20261016-020000_001.zip"}
  }
}
```

JSON was chosen over SQLite to stay dependency free (no cgo), human readable,
and trivially archivable next to the zips. Each index is a full snapshot, so
any single backup directory together with the directories it references is
enough to restore that point in time.

## Quick start with Docker

```bash
cp .env.example .env              # set archive + UI passwords
# edit docker-compose.yaml: mount the directories to back up (read-only)
docker compose up -d --build
```

Open <http://localhost:8080>, go to **配置** and add the sources using their
container paths (e.g. `docs = /data/docs`), or put a `config.yaml` into
`./config/` (see `config/config.example.yaml`).

Behind a restricted network, pass a Go module proxy:
`GOPROXY=https://goproxy.cn,direct docker compose build`.

The container runs as root so it can read all mounted files; mount sources
read-only.

## Configuration

`config.yaml` (path from `-config` or `MYSYNC_CONFIG`; created with defaults if
missing). See `config/config.example.yaml` for all keys:

| key | description |
|---|---|
| `dest` | output directory |
| `sources` | list of `{name, path}`; `name` is the top-level folder in the archives |
| `excludes` | glob patterns; without `/` match any file/dir name, with `/` match the path relative to the source |
| `password` / `encryption` | archive password (empty = none); `aes256` or `zipcrypto` |
| `compression` | `deflate` or `store` |
| `volume_size` | e.g. `500MB`, `4GB`; empty/`0` = no split |
| `max_backup_size` | archive bytes per run, e.g. `50GB`; empty/`0` = unlimited |
| `schedule` | empty = off, `6h` = every 6 hours, `02:30` = daily |
| `ui.username` / `ui.password` | enable basic auth for the web UI |
| `listen` | UI address, default `:8080` |

Environment overrides: `MYSYNC_PASSWORD`, `MYSYNC_UI_USER`, `MYSYNC_UI_PASSWORD`,
`MYSYNC_LISTEN`.

## Backing up with limited disk space

Set `max_backup_size` (e.g. `50GB`) to cap how much a single run writes:

1. Changed files are taken in path order while the estimated archive size
   still fits (the estimate is a conservative upper bound, so the real size
   stays below the cap); files that do not fit are **deferred**, smaller ones
   after them may still be taken.
2. The run is a normal backup directory. Its index records only the archived
   files, so the deferred ones show up again as changes in the next run. The
   UI and logs show how many files / bytes are still pending.
3. Free space (e.g. move the finished backup directory to another disk) and
   run the backup again; repeat until nothing is deferred.

Moving whole backup directories away is fine: `index.latest.json` in the
destination root keeps the incremental state. Keep it, or the next run starts
a full backup. To restore, put all directories of the chain back into one
destination (or extract the volumes manually).

A single file larger than the cap can never be archived; it is reported as
a warning in every run until the cap is raised. Deletions are always recorded.

## CLI

```bash
my_sync serve   -config config.yaml            # web UI + scheduler (default)
my_sync backup  -config config.yaml [-dry-run] # one backup run
my_sync list    -config config.yaml            # list backups
my_sync restore -config config.yaml -to /restore [-id 20261015-020000] [-prefix docs/]
```

`restore` reconstructs the file tree of the latest (or given) backup using the
index: each file is read from the volume holding its latest version, checked
against its SHA-256 and gets its original mtime back.

## Restoring manually

Every volume is a normal zip. Open it with 7-Zip / WinRAR / Bandizip / Keka
(AES-256) or any zip tool (ZipCrypto) and enter the password. To get the latest
state of everything, extract the volumes of all backups in chronological order,
or look up a file in **文件索引** (or `files` in the newest `index.json`) to find
the backup directory and volume that contains it.

## Notes and limitations

- A file is never split across volumes; a file larger than `volume_size` gets a
  volume of its own, which can therefore be larger than the limit.
- Only regular files are backed up; symlinks, devices and empty directories are
  skipped.
- Archives keep file names as UTF-8 (flagged), mtime (incl. extended
  timestamp) and permission bits.
- The destination directory is skipped automatically if it lies inside a source.
- Old backups are never deleted automatically; the newest directory references
  older ones, so prune only whole chains (e.g. start a fresh destination).
  To force a new full backup into the same destination, also remove
  `index.latest.json`.

## Development

```bash
go test -race ./...
go build -o my_sync ./cmd/my_sync
```

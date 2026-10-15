# GManager local API (v0.1 foundation)

The agent listens on **127.0.0.1 only**. There is no CORS. A request whose `Host` is not `127.0.0.1` or `localhost` is rejected with **403**. When `Origin` is sent it must be `http` or `https` on `127.0.0.1` or `localhost` and the port must be the dashboard UI port (default **5173**, `--ui-port`). Any other origin is **403**. An empty `Origin` is allowed.

Callers send `Authorization: Bearer <token>`. The token is `GMANAGER_TOKEN` when that value is at least 32 characters. Otherwise the agent reads `<data-dir>/token`, or creates that file from `crypto/rand` with owner-only permissions. A missing or wrong token is **401**. The process does not log the token. Comparison hashes both sides and uses a constant-time compare.

Paths are namespaced by instance: `/api/v1/servers/{instanceId}/...`. v0.1 has one instance, default id `default`.

Start, stop, restart, install, update, backup, and restore run against the process supervisor. Install and update call the configured SteamCMD runner (`login anonymous`, `app_update 2227360 validate`). The runner is not contacted by tests or CI. Backups are timestamped zips on disk. The log store is in memory.

## Rate limit

Default: **120 requests per minute** per client IP (`DefaultRateLimit`, `DefaultRateWindow`). Authentication is checked first, and a failed check counts toward the same limit so token guessing is throttled. Once the limit is reached the response is **429**, including further wrong tokens. Authenticated requests count too, including a later **403**.

When the limit is exceeded the response is **429** with `Retry-After` set to a whole number of seconds.

```json
{"error":"rate_limited","message":"too many requests"}
```

## Jobs

Writes return **202**:

```json
{"jobId":"<id>"}
```

`Location` is `/api/v1/jobs/<id>`. Poll:

`GET /api/v1/jobs/{id}` → **200** with `id`, `type`, `instanceId`, `state`, `error`, `result`, `createdAt`, `finishedAt`.

`state` is `queued`, `running`, `succeeded`, or `failed`. Unknown ids are **404**. Finished jobs are kept in memory for 15 minutes (`DefaultRetention`) and then dropped, which also returns **404**.

`POST /api/v1/jobs/{id}/cancel` cancels a running job. Install and update pass that context to SteamCMD, so cancel stops the SteamCMD process. A SteamCMD run also stops on its own after `--steamcmd-timeout` (default 60 minutes). The response is **200** with the job, **404** when the id is unknown, and **409** `job_finished` when the job already succeeded or failed. Cancelling an update does not cancel the restart of the previous build.

Config saves are decided before the response (so a conflict is a synchronous **409**, not a failed job). An accepted config save is recorded as a job that is already `succeeded`. Other accepted actions are real jobs.

`start` spawns the module executable and waits for the ready line. It does **not** rewrite config. A missing executable fails the job. `stop` uses the module stop method (unknown for Plains of Pain, so the process is killed) and requires `confirm: true`. `restart` stops, then starts, and requires `confirm: true`. `install` runs SteamCMD into the instance directory and does not start the server. `update` stops, runs the backup hook, snapshots game files except `configs/server.json`, runs SteamCMD, and starts. If the update or the new build fails, the snapshot is restored only when the disk has room for it, and a server that was running is started again. The `gmanager` binary always installs that hook. A process built without a backup store still returns **501** `not_implemented` instead of starting the job. `backup` and `restore` are real jobs, described below.

## Idempotency

Any write accepts `Idempotency-Key` (max 200 characters).

- The same key and the same request (method, path, query, `If-Match`, body) replay the original **202** body and `Location`.
- The same key with a different request is **409** `idempotency_conflict`.
- Keys are stored only for accepted **202** responses. A **400** or **409** can be retried with the same key.
- The store is in memory, entries expire after 15 minutes (`DefaultRetention`), and it is empty after a restart.

## Destructive actions

`POST /api/v1/servers/{instanceId}/stop`, `.../restart`, and `.../restore` require a JSON body with `confirm` set to `true`. Restore also requires `id`. Anything else is **400**:

```json
{"error":"confirm_required","message":"stop requires confirm=true"}
```

Stop, restart, and restore are destructive. The other writes are not. An update with no backup store is **501**, not a failed job. A backup of a running server stops it with the current stop method, which can lose data until a clean Plains of Pain stop is verified.

## Config revision

`GET /api/v1/servers/{instanceId}/config`

```json
{
  "revision": 1,
  "sha256": "<hex>",
  "mtime": "<RFC3339>",
  "size": 0,
  "externalChange": false,
  "raw": "{ ... }",
  "content": {},
  "warnings": [],
  "lastKnown": ""
}
```

`lastKnown` is present only when `externalChange` is true. `warnings` includes non-default Plains of Pain ports. `content` is omitted when the file is not valid JSON; `raw` is still returned.

The agent stores `{revision, sha256, mtime, size}` of the last file it acknowledged, plus a copy of that body with `password` removed. The metadata file keeps a sha256 of the password's raw JSON so a later save can merge against the live file. It does not store the password itself. A later read or write re-hashes the file and compares mtime and size. A difference is an external edit: the revision bumps, the file is not rewritten, and `externalChange` is true.

`PUT /api/v1/servers/{instanceId}/config`

Header: `If-Match: <revision>` (a bare integer or a quoted integer). Body:

```json
{"content": {}, "resolutions": {"serverName": "disk"}}
```

`resolutions` values are `disk` or `edit`.

- Missing or invalid `If-Match`: **400** `if_match_required`.
- Stale revision: **409** `stale_revision` plus the current `revision`. The file is not written.
- Both the on-disk file and the edit changed the same field: **409** `conflict` plus `conflicts` (`field`, `base`, `disk`, `edit`). Nothing is written until each conflict has a resolution.
- Validation failure: **400** `validation_failed` plus `fields`. Nothing is written.
- The on-disk file was deleted: **409** `external_delete`. The agent does not recreate it.
- Success: **202** and a job whose result includes the new `revision`, `sha256`, and `written`.

Merge rules:

- A field only the user changed is taken from the edit.
- A field only the file changed is taken from disk.
- A field both changed to different values conflicts.
- Keys omitted from `content` are kept. Unknown keys survive, and their raw JSON is copied through when the value did not change.
- Key order follows the on-disk file.
- If the merge does not change the on-disk bytes, the file is not rewritten.
- A real write is a temp file plus rename. The previous file is kept as a timestamped `.bak`.

The agent does not write `server.json` when a start job runs.

## Backups

The backup directory is `--backup-dir`. When that flag is empty the directory is `<data-dir>/backups`. It must be absolute, must not be a UNC path, and must not sit inside the server directory (or the other way around). The installer may still suggest `P:\GManager\Backups` when drive `P:` exists; the agent does not select that path on its own.

`--backup-keep` (default **10**) is applied per kind (`manual`, `update`, `safety`). A manual backup is not deleted to make room for an update or a safety zip. Pruning always leaves at least one zip that still verifies, and it never deletes the zip that is about to be restored or the safety zip just written for that restore. A zip that cannot be opened or whose manifest HMAC does not verify is not deleted: it is renamed to `<id>.zip.quarantine` and listed with `corrupt: true` and `quarantined: true`. `POST .../backups/discard` with `confirm: true` and that id is the only way to remove it. A live zip is not discarded. Leftover `*.zip.partial` files are removed when a backup is written or listed. If `backup.hmac` and its copies are all missing while any zip or quarantine file exists, backup and list fail instead of creating a new key.

The save directory is `--user-dir` when that flag is set. Otherwise it is `userdir` from `server.json`. The Plains of Pain default save path is unverified, so a backup with neither value fails the job with `save path unknown` and writes nothing. The agent does not guess a path.

A backup is taken only while the process is stopped. `POST .../backup` on a running or starting server stops it, writes the zip, and starts it again. A server that was already stopped stays stopped. `update` stops first, then calls the same writer, then continues with SteamCMD. The zip is not written while the process is up.

Before the zip is created, the agent checks that the backup volume has room for the files plus a small margin. If it does not, the job fails and no partial file is left behind.

Each zip is named with a UTC timestamp id (`20060102T150405.000000000Z`, plus `-001` and so on if that id is taken). Inside it:

- `manifest.json` with `version`, `id`, `createdAt`, `instanceId`, `gameId`, `kind` (`manual`, `update`, or `safety`), the save indexes, and one record per file
- `saves/<index>/<relative path>` for each save directory
- `config/<relative path>` for each module config file (`configs/server.json` for Plains of Pain)

Each file record has `path`, `sha256` (uncompressed bytes), `size`, `role`, and `rel`. The manifest also carries `hmac`, an HMAC-SHA256 over the canonical manifest. The key is an owner-only file at `<data-dir>/backup.hmac`, with copies at `<data-dir>/backup.hmac.bak` and `<backup-dir>/integrity.key`. None of those files are stored in the zip, so rewriting a hash to match a tampered file does not verify. A version-1 manifest with an empty `hmac` and matching hashes is signed in place with the current key. A wrong `hmac`, or an empty `hmac` whose hashes do not match, is quarantined and is not rewritten. Stripping `hmac` while leaving the hashes unchanged is accepted once, because a process that can already replace files in the backup directory can replace the zip outright. The manifest does not contain file contents. A server password in `server.json` is inside the zip, because restore needs it, and is not copied into the list response or the manifest. The zip file is created with mode `0600`. Paths reject `:`, Windows reserved device names, and names with a trailing dot or space, and two paths that differ only by case. The reader refuses archives that exceed the entry count, uncompressed size, or compression-ratio caps.

`GET /api/v1/servers/{instanceId}/backups?cursor=&limit=`

`cursor` is the last backup id the client has seen. Empty means start at the oldest. `limit` defaults to 100 and must be 1–500. A cursor that is not a backup id is **400** `invalid_cursor`.

```json
{
  "backupDir": "C:\\GManager\\data\\backups",
  "keep": 10,
  "items": [{
    "cursor": "20261015T152301.000000000Z",
    "id": "20261015T152301.000000000Z",
    "createdAt": "2026-10-15T15:23:01Z",
    "bytes": 1200,
    "fileCount": 2,
    "kind": "manual",
    "instanceId": "default"
  }],
  "nextCursor": "20261015T152301.000000000Z"
}
```

Items have ids greater than the request cursor. `nextCursor` is the last item's id, or the request cursor when the page is empty. A zip that cannot be read is still listed, with `corrupt: true`, and does not fail the request. A quarantined zip is listed the same way with `quarantined: true`. No backup store is **501** `not_implemented`. A missing integrity key while backups exist is **500** `backup_list_failed`.

`POST /api/v1/servers/{instanceId}/backups/discard`

```json
{"confirm": true, "id": "20261015T152301.000000000Z"}
```

Missing `confirm: true` is **400** `confirm_required`. A missing id is **400** `backup_id_required`. A malformed id is **400** `invalid_backup_id`. An unknown id is **404** `not_found`. A zip that is still a normal backup is **409** `not_quarantined`. Success is **200** with `action` and `id`.

`POST /api/v1/servers/{instanceId}/backup` accepts any JSON object, including an empty body. The job result includes `action`, `id`, `kind`, `bytes`, `fileCount`, `createdAt`, and `restarted`.

`POST /api/v1/servers/{instanceId}/restore`

```json
{"confirm": true, "id": "20261015T152301.000000000Z"}
```

`confirmArchivedSaveDir: true` is optional. Without it, save bytes are written to the save folder configured right now (`--user-dir`, otherwise `userdir` in the current `server.json`). With it, and only when `--user-dir` is unset, an empty or missing directory named by the archived `server.json` may be used instead. A non-empty directory that is not the current save folder is refused.

Missing `confirm: true` is **400** `confirm_required`. A missing id is **400** `backup_id_required`. A malformed id is **400** `invalid_backup_id`. An unknown id is **404** `not_found`. Those checks happen before a job exists.

The job verifies every sha256 and the manifest HMAC before it stops the server or changes a live file. It then stops the server if it is running, checks again that a non-current destination is still empty, writes a `safety` backup of the destination it is about to replace, verifies that safety zip, and stages every save root and config file. When the restore writes into the current save folder, `server.json`'s `userdir` is rewritten to that folder if the archived value names a different one. If the archived config is not valid JSON or the `userdir` value cannot be made to match, the job fails before any live file is swapped. Those staged copies are swapped into place together. Cancellation is ignored once the swaps start. If a swap fails, the previous files are renamed back first. The verified safety backup is written back only when that rename fails, and the hold copies are kept until that rollback finishes. The editor records the write, so the next config read is not reported as an external edit. A server that was running is started again. A failed restore leaves the safety zip in place. A hash or HMAC mismatch fails the job before the stop, and the live files stay as they were. On startup the agent also finishes an interrupted swap: a missing destination is restored from the newest `.gmanager-hold-*` copy, and leftover `.gmanager-restore-*` staging directories are removed.

Scheduled backups are not in this slice. The writer is the one a scheduler would call.

## Logs

`GET /api/v1/servers/{instanceId}/logs?cursor=&limit=`

`cursor` is the last cursor the client has seen. Empty means start at the beginning. `limit` defaults to 100 and must be 1–500.

```json
{"items":[{"cursor":1,"time":"<RFC3339>","text":"..."}],"nextCursor":1}
```

Items have cursors greater than the request cursor. `nextCursor` is the last item's cursor, or the request cursor when the page is empty.

The store is in memory in this slice. File tailing, websockets, and surviving an agent restart are later work.

## Other reads

- `GET /api/v1/health` → `{"status":"ok","bindHost":"127.0.0.1"}`
- `GET /api/v1/servers` → `{"instances":[{"id","gameId","displayName"}]}`
- `GET /api/v1/servers/{instanceId}/status` → `state` (`stopped`, `starting`, `running`, `crashed`, `updating`), `ready`, `uptimeSeconds`, `pid` (`null` when there is no process), `restarts`, `message`, and `ports` from the module (Plains of Pain defaults UDP 7777 and UDP 27016). `memoryBytes` and `cpuPercent` are present only when the host produced a sample. `running` means the ready line was seen.
- `GET /api/v1/servers/{instanceId}/backups` → cursor-paged backup list, oldest first. See Backups.

## Writes in this slice

| Method and path | Confirm | What the job does |
|---|---|---|
| `POST .../start` | no | Spawns the server and waits until ready. Does not touch config. |
| `POST .../stop` | yes | Stops the process. A requested stop is not a crash. |
| `POST .../restart` | yes | Stop, then start. |
| `POST .../install` | no | Anonymous SteamCMD install. Does not start the server. |
| `POST .../update` | no | Stop, backup, update, start. Restores the previous build on failure. |
| `POST .../backup` | no | Zip saves and config while the server is stopped. A running server is stopped and started again. |
| `POST .../restore` | yes | Verify the manifest, take a safety backup of the destination, restore into the current save folder, and start the server again if it was running. |
| `PUT .../config` | no | Validates, merges, and writes as described above. |

# GManager local API (v0.1 foundation)

The agent listens on **127.0.0.1 only**. There is no CORS. A request whose `Host` is not `127.0.0.1` or `localhost` is rejected with **403**. When `Origin` is sent it must be `http` or `https` on `127.0.0.1` or `localhost` and the port must be the dashboard UI port (default **5173**, `--ui-port`). Any other origin is **403**. An empty `Origin` is allowed.

Callers send `Authorization: Bearer <token>`. The token is `GMANAGER_TOKEN` when that value is at least 32 characters. Otherwise the agent reads `<data-dir>/token`, or creates that file from `crypto/rand` with owner-only permissions. A missing or wrong token is **401**. The process does not log the token. Comparison hashes both sides and uses a constant-time compare.

Paths are namespaced by instance: `/api/v1/servers/{instanceId}/...`. v0.1 has one instance, default id `default`.

Start, stop, restart, install, and update run against the process supervisor. Install and update call the configured SteamCMD runner (`login anonymous`, `app_update 2227360 validate`). The runner is not contacted by tests or CI. Backup and restore are still accepted jobs that record `implemented: false`. Logs are in memory.

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

Config saves are decided before the response (so a conflict is a synchronous **409**, not a failed job). An accepted config save is recorded as a job that is already `succeeded`. Other accepted actions are real jobs.

`start` spawns the module executable and waits for the ready line. It does **not** rewrite config. A missing executable fails the job. `stop` uses the module stop method (unknown for Plains of Pain, so the process is killed) and requires `confirm: true`. `restart` stops, then starts. `install` runs SteamCMD into the instance directory and does not start the server. `update` stops, runs the backup hook, snapshots game files except `configs/server.json`, runs SteamCMD, and starts. If the update or the new build fails, the snapshot is restored and a server that was running is started again. The default backup hook fails closed until a backup implementation exists. `backup` and `restore` still record `implemented: false`.

## Idempotency

Any write accepts `Idempotency-Key` (max 200 characters).

- The same key and the same request (method, path, query, `If-Match`, body) replay the original **202** body and `Location`.
- The same key with a different request is **409** `idempotency_conflict`.
- Keys are stored only for accepted **202** responses. A **400** or **409** can be retried with the same key.
- The store is in memory, entries expire after 15 minutes (`DefaultRetention`), and it is empty after a restart.

## Destructive actions

`POST /api/v1/servers/{instanceId}/stop` and `.../restore` require a JSON body with `confirm` set to `true`. Anything else is **400**:

```json
{"error":"confirm_required","message":"stop requires confirm=true"}
```

These two actions are destructive. The other writes are not.

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

## Writes in this slice

| Method and path | Confirm | What the job does |
|---|---|---|
| `POST .../start` | no | Spawns the server and waits until ready. Does not touch config. |
| `POST .../stop` | yes | Stops the process. A requested stop is not a crash. |
| `POST .../restart` | no | Stop, then start. |
| `POST .../install` | no | Anonymous SteamCMD install. Does not start the server. |
| `POST .../update` | no | Stop, backup hook, update, start. Restores the previous build on failure. |
| `POST .../backup` | no | Recorded, not implemented. |
| `POST .../restore` | yes | Recorded, not implemented. |
| `PUT .../config` | no | Validates, merges, and writes as described above. |

# GManager

GManager is a free, open-source (MIT) Windows game-server manager. A Go agent runs on the same PC as the dedicated server, bound to `127.0.0.1` with a bearer token. A local web dashboard and, later, a PowerShell module talk only to that API.

The first game module is Plains of Pain (SteamCMD app 2227360).

This tree is the foundation plus the process supervisor and SteamCMD installer. It does not install a Windows service or open firewall ports. CI does not download Steam or Plains of Pain.

## License

MIT. See [LICENSE](LICENSE).

`docs/ARCHITECTURE.md` is the v0.1 design draft and still lists the license as an open question. The license of this repository is the MIT file above.

## Docs

- [v0.1 scope](docs/V0.1-SCOPE.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Local API contract](docs/API.md)
- [Unverified game behavior](docs/UNVERIFIED.md)

## Layout

```
agent/          Go module. cmd/gmanager, cmd/fakeserver, supervisor, steamcmd, backups, local API
agent/modules/plainsofpain
web/            Dashboard shell (TypeScript + Vite)
docs/           Scope, architecture, API
powershell/     PowerShell module (later)
installer/      Service installer (later)
site/           Public site (later, only with Terry's OK)
```

## Build and run

Requires Go 1.22 and Node.js 22.

```bash
cd agent
go test ./...
export GMANAGER_TOKEN=dev-placeholder-token-0123456789
go run ./cmd/gmanager --addr 127.0.0.1:8787
```

`GMANAGER_TOKEN` must be at least 32 characters. If it is unset, the agent writes an owner-only token file at `<data-dir>/token` and does not print the token. A shorter environment value or token file is refused. An existing token file that other users can read is tightened, and the agent refuses to start when that cannot be done. Do not commit a real token. `--addr` must be `127.0.0.1`; `0.0.0.0` and other hosts are refused. Wrong tokens count toward the per-IP rate limit.

Defaults:

- data directory: `%ProgramData%\GManager` on Windows, `data/` otherwise
- server directory: `<data-dir>/servers/default/`
- config file: `<data-dir>/servers/default/configs/server.json`
- revision metadata: `<data-dir>/state/default/server.json.meta`
- token file, when generated: `<data-dir>/token`

Flags: `--data-dir`, `--server-dir`, `--instance` (default `default`), `--ui-port` (default `5173`), `--stop-method` (empty, or `interrupt`), `--console-commands` (default false), `--steamcmd-timeout` (default `60m`), `--backup-dir`, `--backup-keep` (default `10`), `--user-dir`. An instance id starts with a letter or digit and may contain letters, digits, `_`, and `-`, up to 64 characters. The API accepts a browser `Origin` only on `--ui-port`.

`--stop-method` defaults to empty, which uses the module. Plains of Pain's clean stop is unknown, so stop kills the process. `interrupt` sends SIGTERM on Unix and is not a confirmed game stop. `--console-commands` adds `-cc`. Leave it off until that flag is confirmed. SteamCMD is stored under `<data-dir>/steamcmd`. Restart and restore require `confirm: true`.

`--backup-dir` is where timestamped zip backups are written. When it is empty the agent uses `<data-dir>/backups`. The directory must be absolute, must not be a UNC path, and must not overlap the server directory. `--backup-keep` keeps that many zips of each kind and always leaves one that still verifies. A zip that cannot be verified is quarantined instead of deleted. `POST .../backups/discard` removes a quarantined zip, and `POST .../backups/release` puts one back. An unsigned zip stays unsigned until `POST .../backups/sign`. That call shows the manifest and file sha256 first; confirming sends the preview digest, and a zip newer than the integrity key is refused. The HMAC key is `<data-dir>/backup.hmac`, copied to `backup.hmac.bak` and `<data-dir>/integrity.key`; if every copy is missing and backups already exist, the agent does not generate a new key. `--user-dir` overrides `userdir` from `server.json`. If neither is set, backup and update fail instead of guessing the Plains of Pain save directory. Restore writes saves into that current folder unless `confirmArchivedSaveDir` is set, rewrites `userdir` to that folder when it differs, and records an owner-only HMAC journal at `<data-dir>/.gmanager-restore-journal-<instance>` before the first swap. The expected restore id is stored per instance. The journal file is removed before that id is rotated. A bad journal or any other startup failure while reading it is logged and blocks only that restore until it is resolved; it does not stop the agent. `POST .../backups/journal` shows the journal. Rollback of a stale id echoes the preview restore id. Discard removes an unusable, finished, or not-yet-swapped journal; a swapped journal that still needs a rollback is refused unless `force` and that restore id are sent, and an unverified discard is logged. `POST .../backups/discard-hold` previews one stray hold and deletes it only when the confirmed size and modification time match and the destination does not look mid-restore. Restore and those journal calls take the same lock. The key's creation time is stored in HMAC'd metadata, and signing uses that time. A server that was running is started again afterwards. Update stops the server, takes a backup, runs SteamCMD, and starts. A failed update puts the previous build back when the disk has room.

Dashboard, in another terminal:

```bash
cd web
npm ci
npm run dev
```

Open `http://127.0.0.1:5173`. The dev server proxies `/api` to `http://127.0.0.1:8787`. Paste the same placeholder token and use **Check agent**. The token stays in the tab and is not saved.

`npm run build` typechecks and writes `web/dist`. Embedding that build in the agent is later work.

## Tests

```bash
cd agent && go test ./...
cd web && npm run build
```

Config validation and the external-edit safeguard are covered in `agent/modules/plainsofpain` and `agent/internal/config`. The API tests cover 202 jobs, idempotency, `If-Match` 409, cursor logs, cursor-paged backups, confirm, and 429. Backup and restore tests use `agent/cmd/fakeserver`, including a running server that is stopped for the zip and started again. CI also runs `go test ./...` on `windows-latest`, which builds and runs the Windows-only files.

Process tests use `agent/cmd/fakeserver`, a small stand-in that prints `Set Game State: RUNNING` and can crash, hang, or exit on an interrupt. Those tests do not contact Steam. SteamCMD tests extract a local archive or run a stand-in script.

## Unknowns (do not guess)

These stay configurable and unresolved until a real Plains of Pain install confirms them. The checklist is [docs/UNVERIFIED.md](docs/UNVERIFIED.md).

| Topic | Behavior in this slice |
|---|---|
| Default save path | `SavePaths` returns an error unless `userdir` or `--user-dir` is set. No default path is invented. |
| Clean stop on Windows | `StopStrategy` method is unknown. Stop kills the process (`TerminateJobObject` on Windows, `SIGKILL` on Unix). |
| `-cc` console flag | Omitted unless the instance sets `EnableConsoleCommands`. Off by default. |
| `adminAccountIDs` format | Preserved verbatim. Not type-checked. Fixtures use `STEAM64_PLACEHOLDER`, never a real Steam ID. |
| `worldId`, `mapId`, `seed`, `ttr` | Preserved verbatim. Types are not guessed. |
| `ttr` vs the scheduler | Not implemented. |

## Not in this slice

- Windows service, installer, firewall consent
- Containing a real Plains of Pain process tree (the fakeserver runs in a job object; the game is unverified)
- Dashboard login (Argon2id, cookies, CSRF) and `reset-password`
- Config form and 3-way diff UI
- Scheduled backups and scheduled restart
- Log file capture and WebSocket tail
- PowerShell module
- Network and CGNAT check
- Reproducible release and `SHA256SUMS`
- Public site

The agent must not rewrite `server.json` on server start, stop, restart, install, or update. Start fails the job if the executable is missing. An unexpected exit restarts with backoff (1s, 5s, 15s, never more than 30s). After 3 crashes in 10 minutes the supervisor halts.

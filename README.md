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
agent/          Go module. cmd/gmanager, cmd/fakeserver, supervisor, steamcmd, local API
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

Flags: `--data-dir`, `--server-dir`, `--instance` (default `default`), `--ui-port` (default `5173`), `--stop-method` (empty, or `interrupt`), `--console-commands` (default false). An instance id starts with a letter or digit and may contain letters, digits, `_`, and `-`, up to 64 characters. The API accepts a browser `Origin` only on `--ui-port`.

`--stop-method` defaults to empty, which uses the module. Plains of Pain's clean stop is unknown, so stop kills the process. `interrupt` sends SIGTERM on Unix and is not a confirmed game stop. `--console-commands` adds `-cc`. Leave it off until that flag is confirmed. SteamCMD is stored under `<data-dir>/steamcmd`. An update refuses to run until a backup hook exists; the agent does not invent one.

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

Config validation and the external-edit safeguard are covered in `agent/modules/plainsofpain` and `agent/internal/config`. The API tests cover 202 jobs, idempotency, `If-Match` 409, cursor logs, confirm, and 429. CI also runs `go test ./...` on `windows-latest`, which builds and runs the Windows-only files.

Process tests use `agent/cmd/fakeserver`, a small stand-in that prints `Set Game State: RUNNING` and can crash, hang, or exit on an interrupt. Those tests do not contact Steam. SteamCMD tests extract a local archive or run a stand-in script.

## Unknowns (do not guess)

These stay configurable and unresolved until a real Plains of Pain install confirms them. The checklist is [docs/UNVERIFIED.md](docs/UNVERIFIED.md).

| Topic | Behavior in this slice |
|---|---|
| Default save path | `SavePaths` returns an error unless `userdir` is set. No default path is invented. |
| Clean stop on Windows | `StopStrategy` method is unknown. Stop kills the process (`TerminateProcess` on Windows, `SIGKILL` on Unix). |
| `-cc` console flag | Omitted unless the instance sets `EnableConsoleCommands`. Off by default. |
| `adminAccountIDs` format | Preserved verbatim. Not type-checked. Fixtures use `STEAM64_PLACEHOLDER`, never a real Steam ID. |
| `worldId`, `mapId`, `seed`, `ttr` | Preserved verbatim. Types are not guessed. |
| `ttr` vs the scheduler | Not implemented. |

## Not in this slice

- Windows service, installer, firewall consent
- Windows Job Object (untested; see the unverified list)
- Dashboard login (Argon2id, cookies, CSRF) and `reset-password`
- Config form and 3-way diff UI
- Backups, restore, scheduled restart
- Log file capture and WebSocket tail
- PowerShell module
- Network and CGNAT check
- Reproducible release and `SHA256SUMS`
- Public site

The agent must not rewrite `server.json` on server start, stop, restart, install, or update. Start fails the job if the executable is missing. An unexpected exit restarts with backoff (1s, 5s, 15s, never more than 30s). After 3 crashes in 10 minutes the supervisor halts.

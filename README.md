# GManager

GManager is a free, open-source (MIT) Windows game-server manager. A Go agent runs on the same PC as the dedicated server, bound to `127.0.0.1` with a bearer token. A local web dashboard and, later, a PowerShell module talk only to that API.

The first game module is Plains of Pain (SteamCMD app 2227360).

This repository is the foundation slice. It does not install a Windows service, download a game server, or open firewall ports.

## License

MIT. See [LICENSE](LICENSE).

`docs/ARCHITECTURE.md` is the v0.1 design draft and still lists the license as an open question. The license of this repository is the MIT file above.

## Docs

- [v0.1 scope](docs/V0.1-SCOPE.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Local API contract](docs/API.md)

## Layout

```
agent/          Go module. cmd/gmanager, core, config editor, local API
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

`GMANAGER_TOKEN` must be at least 32 characters. If it is unset, the agent writes an owner-only token file at `<data-dir>/token` and does not print the token. A shorter environment value or token file is refused. Do not commit a real token. `--addr` must be `127.0.0.1`; `0.0.0.0` and other hosts are refused.

Defaults:

- data directory: `%ProgramData%\GManager` on Windows, `data/` otherwise
- server directory: `<data-dir>/servers/default/`
- config file: `<data-dir>/servers/default/configs/server.json`
- revision metadata: `<data-dir>/state/default/server.json.meta`
- token file, when generated: `<data-dir>/token`

Flags: `--data-dir`, `--server-dir`, `--instance` (default `default`), `--ui-port` (default `5173`). An instance id starts with a letter or digit and may contain letters, digits, `_`, and `-`, up to 64 characters. The API accepts a browser `Origin` only on `--ui-port`.

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

Config validation and the external-edit safeguard are covered in `agent/modules/plainsofpain` and `agent/internal/config`. The API tests cover 202 jobs, idempotency, `If-Match` 409, cursor logs, confirm, and 429.

## Unknowns (do not guess)

These stay configurable and unresolved until a real Plains of Pain install confirms them:

| Topic | Behavior in this slice |
|---|---|
| Default save path | `SavePaths` returns an error unless `userdir` is set. No default path is invented. |
| Clean stop on Windows | `StopStrategy` method is unknown. No signal, window message, or stdin command is assumed. |
| `-cc` console flag | Omitted unless the instance sets `EnableConsoleCommands`. Off by default. |
| `adminAccountIDs` format | Preserved verbatim. Not type-checked. Fixtures use `STEAM64_PLACEHOLDER`, never a real Steam ID. |
| `worldId`, `mapId`, `seed`, `ttr` | Preserved verbatim. Types are not guessed. |
| `ttr` vs the scheduler | Not implemented. |

## Not in this slice

- Windows service, installer, firewall consent
- SteamCMD install and update
- Process supervisor (Job Object), ready-line state, crash restart
- Dashboard login (Argon2id, cookies, CSRF) and `reset-password`
- Config form and 3-way diff UI
- Backups, restore, scheduled restart
- Log file capture and WebSocket tail
- PowerShell module
- Network and CGNAT check
- Reproducible release and `SHA256SUMS`
- Public site

The agent must not rewrite `server.json` on server start. `start` in this slice only returns the module's launch plan and checks that the config file is unchanged.

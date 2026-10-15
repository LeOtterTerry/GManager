# GManager (Game Manager): Architecture, v0.1 FINAL
Status: v0.1 scope final, pending Terry's objections. No repo created, nothing deployed, no messages sent.
Inputs: `/workspace/migration/pack/plains-of-pain.md`, `competitor-map.md` (researched 2026-10-14), Terry's answers relayed by LeOtter - CEO.
Companion: `V0.1-SCOPE.md` (items + acceptance criteria).

## 1. Decisions (locked)
| Topic | Decision |
|---|---|
| Name / repo | **GManager** (Game Manager). Repo name `GManager`, not created yet. |
| License | Open source is planned. **License not yet chosen** (open question). |
| Shape | A local Windows agent (service) does all the work. A local web dashboard and PowerShell module talk to it. The public site is only for landing, docs and downloads. |
| Games | Multi-game via **game modules**; Plains of Pain (PoP) is module #1 and the only one in v0.1. |
| Dashboard auth | Local accounts with **email + password**. Not Steam IDs. Agent bound to **127.0.0.1 only**. |
| Signing | **No paid cert.** Unsigned builds, published SHA256 checksums, reproducible builds, SmartScreen guidance in docs. |
| Install | **Single installer**: no Docker, no Pterodactyl/Wings stack. |
| Public site | `gmanager.leotter.site` (docroot `/home/leotters/public_html/apps/GManager`, PoP pages at `/plains-of-pain`): landing, docs, downloads, Cash App donate link (`$leotterterry`). No control path, no user data. |
| Deploy | Only with Terry's OK; cPanel token goes to the lead builder only at deploy time, via LeOtter - CEO. |

Rejected options: a public cPanel app controlling PCs remotely (impossible without exposing the PC), and a cloud relay (needs persistent websockets plus an account system with a large attack surface; poor fit for shared hosting). Both stay out of scope.

## 2. Target PC (Terry's, reference environment)
Windows 11 Home build 26200; i5-14400F (10c/16t); 48 GB RAM; RTX 5050; C: 614 GB free; P: SSD 962 GB free.
- Server install default: `%ProgramData%\GManager\servers\<instanceId>\` on C:.
- Backup location: **user-chosen drive at first run**; installer suggests `P:\GManager\Backups` when a P: drive exists. Otherwise it suggests a non-system drive, falling back to C:.
- Resources: ~4 GB RAM for 10 players (from research notes). Requirements for 200 players are **unknown**; the hardware has headroom but it is untested.
- Network: Eero mesh. Port forwarding is done in the Eero app (docs). CGNAT status is **unknown** → installer check (§8).

## 3. Verified PoP facts (do not extend without testing)
- Dedicated server: SteamCMD app **2227360**, anonymous login.
- Windows launch: `PlainsOfPain.exe -nographics -batchmode -ignorecompilererrors -config configs/server.json`.
- Ready line: `Set Game State: RUNNING`.
- `configs/server.json` keys: `serverName`, `password` (≤20 chars), `maxPlayers` (**1–200**, configurable), `worldId`, `mapId`, `seed`, `difficulty` (0–4, 255 = custom), `worldSize` (11/21/31/41/51), `port` (7777), `queryPort` (27016), `ttr`, `userdir`, `adminAccountIDs`.
- Network: UDP 7777 + 27016. A custom-port bug has been reported → default ports in v0.1, with a warning if changed.
- **No RCON.** Control is process-level plus config file edits. Custom difficulty values are set by an admin from inside the game client (per the developer guide).
- **Unknown, verify on first install:** default save path, clean-stop method on Windows, whether the `-cc` console-commands flag works on Windows, how `ttr` interacts with the scheduler, the format of `adminAccountIDs` values, and whether the query port exposes the player count.

## 4. Components
1. **GManager Agent** (`gmanager.exe`, Windows service, Go): module host, process supervisor (Job Object), SteamCMD runner, backups, scheduler, log capture, auth, REST + WebSocket API, embedded dashboard. Binds 127.0.0.1.
2. **Local dashboard** (TypeScript + Vite SPA, embedded): login, status, controls, config editor, backups, schedule, live logs, network check.
3. **PowerShell module** `GManager.psm1`: `gm-start`, `gm-stop`, `gm-restart`, `gm-backup`, `gm-status`. Uses the local API only (never touches the process → no races).
4. **Installer** (Inno Setup or WiX, **TBD**): one EXE/MSI. Installs the agent and registers the service, fetches SteamCMD, offers firewall rules (with consent), runs the network/CGNAT check, picks the backup drive, and creates the first admin account.
5. **Public site** (static, PHP only if needed): landing, docs (setup, Eero port forwarding, SmartScreen, verifying checksums), download links to GitHub Releases + SHA256 list, donate link. A client-side `server.json` builder that reuses the module schema is optional (see scope).
6. **Module schemas**: per-game JSON Schema shared by the agent, dashboard and the optional public builder.

## 5. Game-module architecture
The agent core is game-agnostic. Each game is a module compiled into the agent in v0.1 (a static registry; no runtime plugin loading, which keeps it to a single signed-by-checksum binary). A module manifest + Go interface:

```go
type GameModule interface {
    ID() string                     // "plains-of-pain"
    DisplayName() string
    // Install / update
    Install() InstallSpec          // e.g. {Kind: "steamcmd", AppID: 2227360, Anonymous: true, Validate: true}
    ExePath(inst Instance) string  // PlainsOfPain.exe
    // Launch
    LaunchArgs(inst Instance) []string
    StopStrategy() StopSpec        // graceful method + timeout, then kill (PoP: TBD, see §3)
    // Config
    ConfigFiles(inst Instance) []ConfigFile   // path, format (json), JSON Schema
    Validate(cfg []byte) []FieldError          // schema + cross-field rules
    // Readiness / health
    ReadyMatcher() *regexp.Regexp  // PoP: `Set Game State: RUNNING`
    CrashSignals() []*regexp.Regexp // optional
    // Saves
    SavePaths(inst Instance) []string // PoP: from `userdir`, else default (UNKNOWN until verified)
    // Network
    Ports(cfg []byte) []PortSpec   // PoP: UDP port (7777), UDP queryPort (27016)
    Warnings(cfg []byte) []string  // PoP: non-default port warning
}
```
- Core services (supervisor, SteamCMD, backup, scheduler, logs, firewall, network check, API) only call this interface.
- Instance model is multi-instance-ready (`instanceId`, own dir/ports), but **v0.1 ships one instance**.
- Adding a game = new module package + schema + tests. No core changes expected (unverified until a second module exists).
- Later possibility: out-of-process modules or manifests for community games. Post-v0.1, undecided.

## 6. Config editor: never overwrite manual edits
- Agent stores `{revision, sha256, mtime, size}` of the last `server.json` it read or wrote.
- Before every read-for-edit and every write, it re-hashes the file on disk. If the hash differs from the recorded one, the file was **changed externally**:
  - Load the on-disk version as the new base. Show a **3-way diff** (last known / on-disk / user's pending edit).
  - Non-conflicting field changes auto-merge; conflicting fields **prompt** the user to pick. Nothing is ever written silently over external changes.
  - Unknown keys in the file are preserved verbatim (round-trip), and so is the key order where possible.
- A file watcher also flags external changes live in the UI.
- The agent **never rewrites config on server start** (unlike the Legion GPanel behaviour in the competitor map).
- Writes are atomic (temp + rename), with a timestamped `.bak` kept. API saves use `If-Match: <revision>` → `409` on stale.

## 7. Security
- Listener: **127.0.0.1 only**; refuses 0.0.0.0 / LAN binding in v0.1. Agent port is never opened in the firewall.
- **Accounts:** email + password, stored locally (SQLite) with Argon2id hashes. The first admin is created in the installer or on first launch. Sessions use HttpOnly, SameSite=Strict cookies plus a CSRF token. Login is rate-limited/locked out after repeated failures. Roles in v0.1: `admin` only (more roles later).
  - Email is used as the login identifier only. **No outbound email in v0.1**, so password reset is a local CLI command (`gmanager.exe reset-password`, requires admin on the PC).
- **PowerShell/automation:** per-user API token in `%ProgramData%\GManager\tokens\` (ACL: Administrators + service), sent as a Bearer token.
- Strict `Host`/`Origin` checks (stop DNS rebinding and drive-by CSRF); no CORS in v0.1.
- Firewall: only inbound UDP game ports for the server exe, with consent.
- Secrets (server password) are masked in the UI, logs and backups metadata.
- Public site: no secrets, no user data. Donate is a plain outbound link.

## 8. Network & CGNAT check (installer + dashboard)
1. Determine the LAN IP and the default gateway.
2. Public IP: STUN binding request (e.g., public STUN servers) and/or an HTTPS IP-echo lookup. **Which services to use is TBD.** Privacy note in docs.
3. Router WAN IP: try UPnP IGD `GetExternalIPAddress`. **Whether Eero exposes UPnP/this call is unverified**. If unavailable, ask the user to read the WAN IP from the Eero app.
4. Classify:
   - WAN IP in 100.64.0.0/10 or private range, or WAN ≠ public IP → **likely CGNAT/double NAT**: warn, link the docs (ask the ISP for a public IP, or use a tunnel/VPS relay; v0.1 docs only).
   - WAN = public → proceed.
5. **Reachability test** (after forwarding + server running): needs an external prober to send UDP to the public IP:7777/27016. **No prober exists yet.** Options: a small endpoint on the public site (PHP UDP send from cPanel; feasibility on that host **unverified**), or the Steam server-list/query check (**unverified**). Until then, v0.1 reports "not tested" plus manual steps (join from outside the LAN).
- Docs: Eero app → Settings → Network settings → Reservations & port forwarding: reserve the PC's IP, then forward UDP 7777 and 27016. **Menu path must be checked against the current Eero app before publishing.**

## 9. Update / backup / restart
- **Update:** stop → backup → `steamcmd +login anonymous +app_update 2227360 validate +quit` → start. Runs manually or on a schedule. Failure → restart the previous build and surface the error.
- **Backup:** zip of `SavePaths()` + config files to the chosen backup dir (suggest `P:`). Stop/quiesce first (method TBD). Manifest with SHA256. Retention by count/age. Restore stops the server first, with `confirm=true`.
- **Stop:** graceful attempt (method TBD) → timeout → terminate Job Object. Data-loss risk documented until verified.
- **Restart:** crash = exit without a requested stop → backoff (3 in 10 min, then halt + alert in UI). Scheduled restarts use local time (CT on Terry's PC). `ttr` interaction to be tested.

## 10. Releases: unsigned, verifiable
- GitHub Actions build with pinned toolchain versions; Go `-trimpath`, `-buildvcs=false`, fixed `SOURCE_DATE_EPOCH`, and `-ldflags` without timestamps; deterministic SPA build (lockfile, `npm ci`).
- Publish `SHA256SUMS` alongside each release. A CI job rebuilds and compares hashes (reproducibility check). Installer reproducibility (Inno/WiX) is **unverified**; at least the exe inside must reproduce.
- Docs: "Windows protected your PC" → More info → Run anyway, why it appears (no paid cert), and how to verify with `Get-FileHash`.
- Optional later: free signing routes (e.g., SignPath OSS program). Eligibility **unverified**.

## 11. Repo layout (`GManager`, not created)
```
/agent        Go service (cmd/gmanager; internal/{core,supervisor,steamcmd,backup,scheduler,api,auth,config,netcheck,firewall})
/agent/modules/plainsofpain   PoP module + schema + fixtures
/web          dashboard SPA
/site         public site (landing, docs, downloads, donate)
/powershell   GManager.psm1
/installer    Inno Setup or WiX
/docs         setup, Eero port forwarding, CGNAT, SmartScreen, checksums, admin IDs
.github/workflows  build, test, reproducibility check, release + SHA256SUMS
LICENSE (TBD)
```

## 12. Differentiators (from competitor-map.md)
| Differentiator | Status |
|---|---|
| Native, validated PoP config UI that **never overwrites manual edits** | **v0.1** |
| **Single installer**, no Docker/Pterodactyl/Wings | **v0.1** (Windows only) |
| Free and open source | v0.1 (license TBD) |
| Scheduled restarts/updates, backups/rollback, log viewer | v0.1 |
| Linux support (cross-platform installer) | Post-v0.1. Windows first |
| Custom difficulty without joining the game | **Post-v0.1, UNVERIFIED FEASIBILITY.** The dev guide says custom values are set in-game. Whether they can be written to a file is unknown; research needed |
| BepInEx + PoP Server Admin mod install, popserveradmin.json GUI | Post-v0.1 |
| Multi-server/multi-world with automatic port assignment | Post-v0.1 (model is ready; custom-port bug must be resolved first) |
| Discord webhooks | Post-v0.1 |
| Players list / broadcast / kick | Post-v0.1, **blocked by no RCON**; possibly via the PoP Server Admin mod (unverified) |
| Remote/internet control, cloud relay | Out of scope |

## 13. Agent API contract (for tinkabot's MCP plugin)
- Resources: `servers`, `status`, `config`, `backups`, `jobs`, `logs` on the local agent API (127.0.0.1; session cookie or Bearer token).
- Writes (install/update, start, stop, restart, backup, restore, config save) are async and return `202` with a `jobId`; poll `GET /jobs/{id}`.
- Every write accepts an `Idempotency-Key` header; repeating a key returns the original job.
- Config carries a `revision` number; saves must send `If-Match: <revision>` and get `409` on a stale revision (optimistic lock). External file edits bump the revision (§6).
- Logs are cursor-paged: `GET /logs?cursor=&limit=` returns `items` and `nextCursor`.
- Documented rate limits; when one is exceeded the API returns `429` with `Retry-After`.
- Destructive operations (`stop`, `restore`) require an explicit `confirm=true` field and are flagged as destructive in the API docs.
- Paths are namespaced per module/instance (e.g., `/api/v1/servers/{instanceId}/...`) for multi-game later.

## 14. Open questions / unknowns
- License choice (MIT / Apache-2.0 / GPL-3.0?).
- GitHub access to create `GManager` under LeOtterTerry (private until release, or public from day one?).
- Terry's Steam account ID for `adminAccountIDs` (still needed even though dashboard auth isn't Steam-based).
- Installer tool (Inno vs WiX); STUN/IP-echo providers; the external UDP prober.
- Eero: UPnP availability, current menu path; CGNAT status of Terry's ISP.
- PoP: save path, clean stop, `-cc` on Windows, `ttr`, query-port player count, resources at 200 players.
- Whether the public site includes the client-side config builder in v0.1 (proposed: stretch).

# Unverified Plains of Pain behavior

Terry does not have a Plains of Pain install. CI does not download the dedicated server and does not contact Steam. The process supervisor is tested against `agent/cmd/fakeserver`. That binary prints a boot log including `Set Game State: RUNNING`, and it can be told to crash, hang, or exit on an interrupt. It is not the game.

Someone with a real install needs to confirm the rows below. Until then the agent keeps the conservative behavior in the last column.

| Item | What is unverified | Current behavior |
|---|---|---|
| Default save path | Where the dedicated server writes saves when `userdir` is empty. | `SavePaths` returns an error unless `userdir` is set. No default path is invented. |
| Clean stop on Windows | Whether a window message, stdin command, or other request stops `PlainsOfPain.exe` without killing it. | The module stop method is unknown. Stop kills the process (`TerminateProcess` on Windows, `SIGKILL` on Unix). That can lose data. |
| `SIGTERM` / `--stop-method interrupt` | Whether an interrupt is a clean Plains of Pain stop. | Not used unless the operator sets `--stop-method interrupt`. On Unix that sends `SIGTERM` to the process group. On Windows the interrupt is refused and, when the module allows it, the process is killed. This is not a confirmed game stop. |
| Windows Job Object | Whether a job object is the right way to contain the dedicated server and its children. | Not created. Shipping an untested job object would pretend a Windows behavior we have not seen. |
| `-cc` | Whether `-cc` works on Windows. | Omitted unless `--console-commands` is set. Default is off. |
| `adminAccountIDs` format | The real shape of an admin id. | Preserved verbatim. Not type-checked. Fixtures use `STEAM64_PLACEHOLDER`, never a real Steam ID. |
| `worldId`, `mapId`, `seed`, `ttr` | JSON types. | Preserved verbatim. Types are not guessed. |
| `ttr` vs the scheduler | Whether `ttr` already schedules restarts. | The agent scheduler is not implemented. `ttr` is left untouched. |

SteamCMD `+force_install_dir` is a SteamCMD flag so the dedicated server lands in the instance directory. It is not a claim about the game.

A real-install pass should record, for each row, what was tried and what the server did. Do not replace an unknown with a guess in the meantime.

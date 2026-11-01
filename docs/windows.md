# Running a node on Windows

Windows 10 and 11, on amd64 or arm64. Everything below runs as your own
user in PowerShell; none of it needs administrator rights.

## 1. A backend with a model

Install [Ollama for Windows](https://ollama.com/download/windows). It runs
in the system tray and starts at login. Then pull a model that fits your
GPU's VRAM (or system RAM without a GPU):

```powershell
ollama pull qwen3:8b
```

`qwen3:4b` for 6–8 GB, `qwen3:8b` for 10–12 GB, `qwen3:14b` or
`gemma3:12b` for 16 GB, `qwen3:30b` for 24 GB and up. On NVIDIA, keep the
driver current; Ollama uses CUDA by itself.

**Raise Ollama's context window.** Ollama serves models with a small
window by default (4096 tokens on Windows in our tests), and the network
shows consumers the window your node actually serves. Web search and long
chats need room:

```powershell
setx OLLAMA_CONTEXT_LENGTH 16384
```

then quit Ollama from the tray icon and start it again from the Start
menu.

LM Studio and llama.cpp work as backends too; see
[backends.md](backends.md). The zero-setup `--managed` mode is not
available on Windows yet.

## 2. A node token

Sign up at [spine.remote.network](https://spine.remote.network), open
**User Settings → Node Tokens** and mint one. It starts with `rn_nt_` and
is shown once.

## 3. Install the agent

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.ps1 | iex
```

This downloads the binary for your CPU, checks it against the release's
`checksums.txt`, installs it as `%USERPROFILE%\.local\bin\remote.exe` and
adds that folder to your user PATH. Open a new terminal afterwards so
`remote` is found. `$env:REMOTE_VERSION = "v0.1.1"` before the command
pins a release.

By hand instead: download `remote-windows-amd64.exe` (or `-arm64.exe`)
and `checksums.txt` from the
[releases page](https://github.com/dzunic-systems/remote.network/releases/latest),
compare `(Get-FileHash remote-windows-amd64.exe).Hash` with its line in
`checksums.txt`, and save it as `%USERPROFILE%\.local\bin\remote.exe`.

## 4. Try it

```powershell
remote node bench
```

lists the models the agent found and measures each one. Then, to watch a
node join in the foreground:

```powershell
remote node start `
  --router wss://spine.remote.network/node/v1/connect `
  --router-root qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a `
  --node-token rn_nt_…
```

Look for `authenticated with router` and then `serving models=…`. Ctrl-C
drains in-flight requests and exits.

## 5. Run it as a scheduled task

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/setup-windows.ps1 | iex
```

It asks for the token (input hidden), saves it to
`%USERPROFILE%\.remote\node-token` (readable by you and SYSTEM only) and
registers a scheduled task, `remote-node`, that:

- starts at logon and keeps running after the terminal closes,
- keeps running on battery, with no time limit,
- restarts the agent after a crash (3 tries, a minute apart),
- runs whether or not you are logged on, where Windows allows it (a local
  or domain account; with a Microsoft/Entra-only account it falls back to
  running while you are logged on).

The task runs `%USERPROFILE%\.remote\node-start.ps1`, which reads the token
file; the token is never stored in the task itself. Re-run the setup any
time to change options; it keeps the saved token.

Options need the script as a file:

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/setup-windows.ps1 -OutFile setup-windows.ps1
powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -Models qwen3:8b, gemma3:12b
powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -ExtraArgs "--max-concurrent 2"
```

| | |
|---|---|
| Logs | `Get-Content $env:USERPROFILE\.remote\logs\agent.log -Wait -Tail 50 -Encoding UTF8` |
| State | `Get-ScheduledTask remote-node` (want `Running`) |
| Restart | `Stop-ScheduledTask remote-node; Start-ScheduledTask remote-node` |
| Remove | `powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -Uninstall` |

`Stop-ScheduledTask` ends the agent at once, without draining in-flight
requests. The network fails those over to other nodes, so it is safe, just
not graceful.

## Keep the PC available

- **Don't sleep:** Settings → System → Power → Screen and sleep → "When
  plugged in, put my device to sleep after: Never". Or
  `powercfg /change standby-timeout-ac 0`.
- **Start after a reboot without a login**, if the task fell back to
  logon-only: enable automatic sign-in (Sysinternals Autologon), or
  sign in after each reboot.
- **VPNs:** a full-tunnel VPN on the node can make its connection drop
  repeatedly. If the log shows the router session ending every few
  minutes, try without it.

A sleeping node is not lost: the network marks it offline, routes around
it, and it rejoins on wake.

## Update

Re-run the installer (it stops the running agent to replace the binary),
then start the task again:

```powershell
irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.ps1 | iex
Start-ScheduledTask remote-node
```

The node keeps its identity (`%USERPROFILE%\.remote\node.key`) across
updates.

## Uninstall

```powershell
powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -Uninstall
Remove-Item $env:USERPROFILE\.local\bin\remote.exe
Remove-Item -Recurse $env:USERPROFILE\.remote     # forgets this node's identity and token
```

Revoke the token under **User Settings → Node Tokens** if no other machine
uses it. (Revoking stops new machines from joining with it; nodes already
claimed stay yours.)

Problems: [troubleshooting.md](troubleshooting.md).

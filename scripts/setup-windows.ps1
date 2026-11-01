# setup-windows.ps1 - run the remote.network node agent as a durable Windows
# scheduled task, "remote-node": starts at logon, keeps running after the
# terminal closes, runs on battery, restarts after a crash, no time limit.
# No admin rights needed. Run install.ps1 first.
#
#   irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/setup-windows.ps1 | iex
#
# With options, download it and run it as a file:
#   powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -Models qwen3:8b, gemma3:12b
#   powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -ExtraArgs "--max-concurrent 2"
#   powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -Uninstall
#
# The node token lives in %USERPROFILE%\.remote\node-token (readable by you
# and SYSTEM only), never in the task definition; the task runs
# %USERPROFILE%\.remote\node-start.ps1. Re-run to change options: an
# existing token is kept. $env:REMOTE_NODE_TOKEN skips the prompt.
# Plain ASCII on purpose (see install.ps1).
param(
  [string]$RouterUrl = "wss://spine.remote.network/node/v1/connect",
  [string[]]$Models = @(),
  [string]$ExtraArgs = "",
  [string]$ExePath = "",
  [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
# The network's root public key: routers certified by it are trusted, so
# router key rotations need nothing from the node.
$ROOT  = "qnxkjlvbxsshgj6pifmz7pbhx56nv5ar542dmzmp2sxszak7r32a"
$H     = Join-Path $env:USERPROFILE ".remote"
$logs  = Join-Path $H "logs"
$tf    = Join-Path $H "node-token"
$start = Join-Path $H "node-start.ps1"

if ($Uninstall) {
  Stop-ScheduledTask -TaskName remote-node -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName remote-node -Confirm:$false -ErrorAction SilentlyContinue
  Write-Host "task removed. The node identity and token stay in $H (delete it to forget this node)."
  return
}

if (-not $ExePath) {
  $cmd = Get-Command remote.exe -ErrorAction SilentlyContinue
  if ($cmd) { $ExePath = $cmd.Source } else { $ExePath = Join-Path $env:USERPROFILE ".local\bin\remote.exe" }
}
if (-not (Test-Path $ExePath)) { throw "agent not found at $ExePath - run install.ps1 first, or pass -ExePath <path>" }
Write-Host "agent:  $ExePath ($(& $ExePath --version))"
Write-Host "router: $RouterUrl"

# Preflight: managed llama-server mode is macOS/Linux only, so a Windows node
# needs a backend already running: Ollama by default.
if (-not $ExtraArgs.Contains("--backend-url") -and -not (Test-Path (Join-Path $H "backends.json"))) {
  try {
    Invoke-WebRequest -UseBasicParsing http://localhost:11434/api/tags -TimeoutSec 3 | Out-Null
  } catch {
    Write-Warning "Ollama is not answering on localhost:11434 - install/start it (https://ollama.com). Without a backend the agent exits and the task gives up after 3 restarts; once Ollama runs: Start-ScheduledTask remote-node"
  }
}

New-Item -ItemType Directory -Force $H, $logs | Out-Null
$sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value

# The token: from the environment, else the one already saved, else ask.
$tok = $env:REMOTE_NODE_TOKEN
if (-not $tok -and (Test-Path $tf)) {
  $tok = (Get-Content -Raw $tf).Trim()
  if ($tok) { Write-Host "token:  keeping the one in $tf" }
}
if (-not $tok) {
  $sec = Read-Host -AsSecureString "node token (rn_nt_..., from User Settings -> Node Tokens)"
  $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
  try { $tok = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
  finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
}
if (-not $tok.StartsWith("rn_nt_")) { throw "not a node token (they start with rn_nt_)" }
Set-Content -Path $tf -Value $tok -NoNewline -Encoding ascii
Remove-Variable tok
icacls $tf /inheritance:r /grant:r "*${sid}:(R,W)" "*S-1-5-18:(F)" | Out-Null

$modelArgs = ($Models | ForEach-Object { " --model $_" }) -join ""
if ($ExtraArgs) { $modelArgs += " $ExtraArgs" }
@"
# Started by the remote-node scheduled task (setup-windows.ps1). The token stays in node-token.
`$tok = (Get-Content -Raw '$tf').Trim()
# /s /c ""..."": cmd strips exactly the outer quote pair - the inner ones survive.
`$line = '/s /c ""$ExePath" node start --router $RouterUrl --router-root $ROOT --node-token ' + `$tok + '$modelArgs >> "$logs\agent.log" 2>&1"'
`$p = Start-Process -FilePath cmd.exe -ArgumentList `$line -NoNewWindow -Wait -PassThru
exit `$p.ExitCode
"@ | Set-Content -Path $start -Encoding UTF8

$action   = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$start`""
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
# Use the SID: on Entra-joined machines $env:USERDOMAIN is the cloud tenant
# and a name-based principal fails with "no mapping ... security IDs".
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType S4U

# Idempotent: replace any previous registration.
Stop-ScheduledTask -TaskName remote-node -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName remote-node -Confirm:$false -ErrorAction SilentlyContinue
try {
  Register-ScheduledTask remote-node -Action $action -Settings $settings -Principal $principal -Trigger (New-ScheduledTaskTrigger -AtLogOn) | Out-Null
} catch {
  # S4U needs a local/AD account (Entra-only accounts can't do S4U logons):
  # fall back to logon-only, so the agent runs while you are logged in.
  Write-Warning "S4U principal refused ($($_.Exception.Message)) - falling back to logon-only."
  Register-ScheduledTask remote-node -Action $action -Settings $settings -Trigger (New-ScheduledTaskTrigger -AtLogOn) | Out-Null
}

Start-ScheduledTask remote-node
Start-Sleep -Seconds 3   # Start-ScheduledTask is async; give it a moment
$state = (Get-ScheduledTask remote-node).State
Write-Host "task state: $state (want Running)"
if ($state -ne "Running") {
  Write-Warning "not Running - LastTaskResult: $((Get-ScheduledTaskInfo remote-node).LastTaskResult)"
}
Write-Host "first start probes and benchmarks every model, which can take a few minutes."
Write-Host ""
Write-Host "logs:    Get-Content '$logs\agent.log' -Wait -Tail 50 -Encoding UTF8"
Write-Host "restart: Stop-ScheduledTask remote-node; Start-ScheduledTask remote-node"
Write-Host "remove:  setup-windows.ps1 -Uninstall"
Write-Host "check:   remote node status, and your models in the Catalog at https://spine.remote.network"

# setup-windows.ps1 — register the remote.network node agent as a durable
# Windows scheduled task: survives closed terminals/logouts, starts at logon,
# runs on battery, auto-restarts after crashes, no time limit.
#
# Copy this file anywhere on the laptop, then:
#   powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -RouterUrl wss://spine.remote.network/node/v1/connect
#   powershell -ExecutionPolicy Bypass -File .\setup-windows.ps1 -RouterUrl wss://spine.remote.network/node/v1/connect -Models gemma4:e2b-it-qat, gemma4:e4b-it-qat
# Defaults match the field layout: binary in %USERPROFILE%\.local\bin, logs in
# %USERPROFILE%\.remote\logs.
param(
  [Parameter(Mandatory = $true)]
  [string]$RouterUrl,
  [string]$ExePath  = "",
  [string[]]$Models = @()
)

$ErrorActionPreference = "Stop"

# Find the newest agent binary in %USERPROFILE%\.local\bin (the field
# convention, matching the Unix ~/.local/bin) unless one was given.
if (-not $ExePath) {
  $binDir = Join-Path $env:USERPROFILE ".local\bin"
  $candidate = Get-ChildItem -Path $binDir -Filter "remote-*-windows-amd64.exe" -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if ($candidate) { $ExePath = $candidate.FullName }
}
if (-not $ExePath -or -not (Test-Path $ExePath)) {
  Write-Error "agent binary not found in $env:USERPROFILE\.local\bin — scp it there or pass -ExePath <path>"
}

# Preflight: the managed llama-server path is darwin/linux only, so Windows
# nodes need Ollama up, or the agent exits "no backend could be probed".
try {
  Invoke-WebRequest -UseBasicParsing http://localhost:11434/api/tags -TimeoutSec 3 | Out-Null
} catch {
  Write-Warning "Ollama not reachable at localhost:11434 — install/start it (https://ollama.com), then re-run."
}

$logs = Join-Path $env:USERPROFILE ".remote\logs"
New-Item -ItemType Directory -Force $logs | Out-Null

$modelsArgs = ""
foreach ($m in $Models) { $modelsArgs += " --model $m" }
# /s /c ""…"": cmd strips exactly the outer quote pair — the inner ones survive.
# Plain /c with multiple quotes strips the FIRST and LAST and mangles the line.
$argString = '/s /c ""{0}" node start{1} --router {2} >> "{3}\agent.log" 2>&1"' -f $ExePath, $modelsArgs, $RouterUrl, $logs

$action    = New-ScheduledTaskAction -Execute "cmd.exe" -Argument $argString
$settings  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
# Use the SID: on Entra-joined machines $env:USERDOMAIN is the cloud tenant
# and New-ScheduledTaskPrincipal would fail with "no mapping ... security IDs".
$sid       = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType S4U

# Idempotent: replace any previous registration.
Unregister-ScheduledTask -TaskName remote-node -Confirm:$false -ErrorAction SilentlyContinue
try {
  Register-ScheduledTask remote-node -Action $action -Settings $settings -Principal $principal -Trigger (New-ScheduledTaskTrigger -AtLogOn) | Out-Null
} catch {
  # S4U needs a local/AD account (Entra-only accounts can't do S4U logons) —
  # fall back to logon-only: the agent then runs while the user is logged in.
  Write-Warning "S4U principal refused ($($_.Exception.Message)) — falling back to logon-only."
  Register-ScheduledTask remote-node -Action $action -Settings $settings -Trigger (New-ScheduledTaskTrigger -AtLogOn) | Out-Null
}

Start-ScheduledTask remote-node
Start-Sleep -Seconds 3   # Start-ScheduledTask is async; give cmd a moment
$state = (Get-ScheduledTask remote-node).State
Write-Host "task state: $state (want Running)"
if ($state -ne "Running") {
  Write-Warning "not Running — LastTaskResult: $((Get-ScheduledTaskInfo remote-node).LastTaskResult)"
}

$router = ([Uri]$RouterUrl).Authority
Write-Host ""
Write-Host "tail the log:  Get-Content '$logs\agent.log' -Wait -Tail 50 -Encoding UTF8"
Write-Host "verify online: curl.exe http://$router/api/v1/nodes"
Write-Host "stop / remove: Stop-ScheduledTask remote-node / Unregister-ScheduledTask remote-node"
Write-Host "               (both are hard kills — they skip the graceful drain; the router's"
Write-Host "                reaper marks the node offline and in-flight requests fail over)"

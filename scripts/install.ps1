# install.ps1 - remote.network node-agent installer for Windows. Downloads
# the release binary for this machine, checks it against the release's
# checksums.txt, installs it as %USERPROFILE%\.local\bin\remote.exe and puts
# that folder on your user PATH. No admin rights needed.
#
#   irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/install.ps1 | iex
#
# $env:REMOTE_VERSION pins a release (e.g. v0.1.1; default: the latest).
# Plain ASCII on purpose: Windows PowerShell 5.1 reads a BOM-less file as
# Windows-1252, and a non-ASCII dash decodes to a quote that breaks the parse.

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # Invoke-WebRequest is 10x slower with the progress bar
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = "https://github.com/dzunic-systems/remote.network"
if ($env:REMOTE_VERSION) { $base = "$repo/releases/download/$($env:REMOTE_VERSION)" }
else { $base = "$repo/releases/latest/download" }

switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { $arch = "amd64" }
  "ARM64" { $arch = "arm64" }
  default { throw "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}
$asset = "remote-windows-$arch.exe"
$bin   = Join-Path $env:USERPROFILE ".local\bin"
$exe   = Join-Path $bin "remote.exe"

$tmp = Join-Path $env:TEMP ("rn-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force $tmp | Out-Null
try {
  Write-Host "downloading $asset from $base"
  Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile "$tmp\$asset"
  Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
  $want = Get-Content "$tmp\checksums.txt" | Where-Object { ($_ -split '\s+')[1] -eq $asset } |
    ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1
  $got = (Get-FileHash -Algorithm SHA256 "$tmp\$asset").Hash.ToLower()
  if (-not $want -or $want -ne $got) { throw "checksum mismatch for ${asset}: want '$want' got '$got' - not installed" }
  Write-Host "checksum OK"

  New-Item -ItemType Directory -Force $bin | Out-Null
  # A running agent locks remote.exe; stop it first (setup-windows.ps1's task
  # restarts it afterwards: Start-ScheduledTask remote-node).
  $running = Get-Process -Name remote -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe }
  if ($running) {
    Write-Host "stopping the running agent to replace it"
    Stop-ScheduledTask -TaskName remote-node -ErrorAction SilentlyContinue
    $running | Stop-Process -Force
    Start-Sleep -Seconds 2
  }
  Copy-Item -Force "$tmp\$asset" $exe
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
Write-Host "installed: $exe ($(& $exe --version))"

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not (($userPath -split ";") -contains $bin)) {
  [Environment]::SetEnvironmentVariable("Path", (($userPath.TrimEnd(";") + ";" + $bin).TrimStart(";")), "User")
  Write-Host "added $bin to your user PATH (open a new terminal to use 'remote')"
}
$env:Path = "$env:Path;$bin"

Write-Host ""
Write-Host "next:"
Write-Host "  1. install Ollama (https://ollama.com) and pull a model, e.g. ollama pull qwen3:8b"
Write-Host "  2. sign up at https://spine.remote.network and mint a node token"
Write-Host "     (User Settings -> Node Tokens)"
Write-Host "  3. remote node bench        # what this machine serves, and how fast"
Write-Host "  4. run it as a scheduled task (starts at logon, restarts on crash):"
Write-Host "       irm https://raw.githubusercontent.com/dzunic-systems/remote.network/main/scripts/setup-windows.ps1 | iex"
Write-Host "guide: https://github.com/dzunic-systems/remote.network/blob/main/docs/windows.md"

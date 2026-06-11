# Install the `vault` CLI on Windows from GitHub Releases — no Go, no runtime.
#
#   irm https://raw.githubusercontent.com/l1mzh0317/vault-cli/main/cli/install.ps1 | iex
#
# Env overrides (set before running):
#   $env:INSTALL_DIR = 'C:\tools\vault'   # where to put vault.exe (default: %LOCALAPPDATA%\Programs\vault)
#   $env:VERSION     = 'cli-v0.6.0'        # a specific release tag (default: latest)
#   $env:NO_SKILL    = '1'                 # skip installing the skill

$ErrorActionPreference = 'Stop'
$repo = 'l1mzh0317/vault-cli'

# arch
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'ARM64' { 'arm64' }
  default { 'amd64' }
}
$asset = "vault-windows-$arch.exe"

# install dir (on PATH ideally)
$dir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\vault' }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$bin = Join-Path $dir 'vault.exe'

# download binary
if ($env:VERSION) {
  $url = "https://github.com/$repo/releases/download/$($env:VERSION)/$asset"
} else {
  $url = "https://github.com/$repo/releases/latest/download/$asset"
}
Write-Host "Downloading $asset -> $bin"
Invoke-WebRequest -Uri $url -OutFile $bin -UseBasicParsing

Write-Host "[OK] installed: $bin"

# install the bundled vault skill from the binary (offline, version-matched)
if ($env:NO_SKILL -ne '1') {
  & $bin skill
}

# add the install dir to the USER PATH (persistent) and the current session.
# Use [Environment] on the User scope (not setx — setx truncates PATH at 1024).
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $dir)) {
  $newPath = if ($userPath) { "$userPath;$dir" } else { $dir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  Write-Host "[OK] added $dir to your user PATH"
}
# make `vault` usable in THIS session immediately
if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$env:Path;$dir" }

& $bin version
Write-Host "Open a NEW terminal (or run: `$env:Path += ';$dir') so 'vault' is found everywhere."

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

# install the vault skill (markdown only) so Claude knows the CLI exists
if ($env:NO_SKILL -ne '1') {
  $skillDir = Join-Path $env:USERPROFILE '.claude\skills\vault'
  New-Item -ItemType Directory -Force -Path $skillDir | Out-Null
  try {
    Invoke-WebRequest -Uri "https://raw.githubusercontent.com/$repo/main/cli/skill/SKILL.md" `
      -OutFile (Join-Path $skillDir 'SKILL.md') -UseBasicParsing
    Write-Host "[OK] skill -> $skillDir\SKILL.md  (restart Claude Code to load /vault)"
  } catch {
    Write-Host "  (skill download skipped - binary still works)"
  }
}

# PATH hint
$paths = $env:PATH -split ';'
if ($paths -notcontains $dir) {
  Write-Host "  note: $dir is not on your PATH. Add it, e.g.:"
  Write-Host "    setx PATH `"`$env:PATH;$dir`""
}

& $bin version

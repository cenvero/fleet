# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Cenvero / Shubhdeep Singh

param(
  [Parameter(Mandatory = $true)]
  [int] $ExpectedMajor
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($PSVersionTable.PSVersion.Major -ne $ExpectedMajor) {
  throw "expected PowerShell $ExpectedMajor, got $($PSVersionTable.PSVersion)"
}

# Resolve the currently published stable version first and pin this test
# invocation to it, avoiding a race if the channel advances mid-run.
$manifest = Invoke-RestMethod -Uri "https://fleet.cenvero.org/manifest.json"
$expectedVersion = [string]$manifest.channels.stable.version
if ($expectedVersion -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+') {
  throw "stable manifest returned an invalid version: $expectedVersion"
}

$installPath = Join-Path $HOME ".local\bin\fleet.exe"
Remove-Item -LiteralPath $installPath -Force -ErrorAction SilentlyContinue

# Exclude package-manager directories so the installer must use its pinned
# minisign bootstrap, matching a clean Windows host.
$env:PATH = "$env:SystemRoot\System32;$env:SystemRoot"
if (Get-Command minisign -ErrorAction SilentlyContinue) {
  throw "test setup unexpectedly found minisign"
}
$env:FLEET_CHANNEL = "stable"
$env:FLEET_VERSION = $expectedVersion
$env:FLEET_SKIP_PATH_UPDATE = "1"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
& (Join-Path $repositoryRoot "public\install.ps1")

if (-not (Test-Path -LiteralPath $installPath -PathType Leaf)) {
  throw "installer did not create fleet.exe"
}

$versionOutput = (& $installPath --version 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0) {
  throw "installed fleet.exe --version failed with exit code $LASTEXITCODE"
}
$expectedNumber = $expectedVersion.TrimStart('v')
if ($versionOutput -notmatch [Regex]::Escape($expectedNumber)) {
  throw "installed version mismatch: expected $expectedVersion, got: $versionOutput"
}

$helpOutput = (& $installPath --help 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $helpOutput -notmatch 'Cenvero Fleet') {
  throw "installed fleet.exe failed its command smoke test"
}

# The bootstrap verifier must remain temporary; it must not be installed or
# added to PATH after Fleet verification completes.
if (Get-Command minisign -ErrorAction SilentlyContinue) {
  throw "temporary minisign verifier leaked into PATH"
}

Write-Host "Installed and executed $versionOutput with PowerShell $($PSVersionTable.PSVersion)"

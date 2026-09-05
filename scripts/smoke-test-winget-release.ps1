# SPDX-License-Identifier: AGPL-3.0-or-later

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$ManifestDirectory,

    [Parameter(Mandatory = $true)]
    [string]$ExpectedVersion
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$winget = Get-Command winget -ErrorAction Stop
$manifestPath = (Resolve-Path -LiteralPath $ManifestDirectory -ErrorAction Stop).Path

& $winget.Source validate $manifestPath
$validationExitCode = $LASTEXITCODE
# WINGET_CONFIG_ERROR_MANIFEST_VALIDATION_WARNING means validation completed
# with warnings only. All other nonzero results remain fatal.
$validationWarningExitCode = -1978335192 # 0x8A150028 as signed int32
if ($validationExitCode -ne 0 -and $validationExitCode -ne $validationWarningExitCode) {
    throw "winget validate failed: $validationExitCode"
}

& $winget.Source settings --enable LocalManifestFiles
if ($LASTEXITCODE -ne 0) {
    throw "could not enable local manifest files: $LASTEXITCODE"
}

$installAttempted = $false
$operationFailure = $null
$cleanupFailure = $null
try {
    $installAttempted = $true
    & $winget.Source install --manifest $manifestPath --silent --accept-package-agreements --accept-source-agreements --disable-interactivity
    if ($LASTEXITCODE -ne 0) {
        throw "local WinGet install failed: $LASTEXITCODE"
    }

    $fleet = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links\fleet.exe'
    if (-not (Test-Path -LiteralPath $fleet)) {
        $packages = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages'
        $fleet = Get-ChildItem -LiteralPath $packages -Filter fleet.exe -Recurse -ErrorAction SilentlyContinue |
            Select-Object -First 1 -ExpandProperty FullName
    }
    if (-not $fleet) {
        throw 'WinGet install did not create or install fleet.exe'
    }

    $reported = ((& $fleet --version) -join "`n").Trim()
    $expected = "Cenvero Fleet $ExpectedVersion"
    if ($LASTEXITCODE -ne 0 -or $reported -cne $expected) {
        throw "installed Fleet version mismatch: expected '$expected', got '$reported'"
    }
}
catch {
    $operationFailure = $_
}
finally {
    if ($installAttempted) {
        try {
            & $winget.Source uninstall --id Cenvero.Fleet --exact --silent --accept-source-agreements --disable-interactivity
            if ($LASTEXITCODE -ne 0) {
                $cleanupFailure = "local WinGet uninstall failed: $LASTEXITCODE"
            }
        }
        catch {
            $cleanupFailure = "local WinGet uninstall failed: $($_.Exception.Message)"
        }
    }
}

if ($operationFailure) {
    if ($cleanupFailure) {
        throw "$($operationFailure.Exception.Message) Cleanup also failed: $cleanupFailure"
    }
    throw $operationFailure
}
if ($cleanupFailure) {
    throw $cleanupFailure
}

$ErrorActionPreference = "Stop"

$BaseUrl = "https://fleet.cenvero.org"
$Channel = if ($env:FLEET_CHANNEL) { $env:FLEET_CHANNEL } else { "stable" }
$VersionOverride = $env:FLEET_VERSION
$MinisignPublicKey = "RWRb53p9WTsWCO2RZT3bvjrZw4QjXnIo2R7NUqhPsfvhR8u0sS55hZb3"

$archMap = @{
  "AMD64" = "amd64"
  "ARM64" = "arm64"
}

$arch = $archMap[$env:PROCESSOR_ARCHITECTURE]
if (-not $arch) {
  throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
}

$target = "windows-$arch"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("fleet-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
  $manifestPath = Join-Path $tmp "manifest.json"
  Invoke-WebRequest -Uri "$BaseUrl/manifest.json" -OutFile $manifestPath
  $manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json

  if ($VersionOverride) {
    $version = $VersionOverride
  } else {
    $version = $manifest.channels.$Channel.version
    if (-not $version) {
      throw "Channel not found: $Channel"
    }
  }

  $binary = $manifest.binaries.$version.$target
  if (-not $binary.url) {
    if ($VersionOverride) {
      $archiveName = "fleet_${version}_windows_${arch}.zip"
      $binary = [PSCustomObject]@{
        url           = "https://github.com/cenvero/fleet/releases/download/${version}/${archiveName}"
        signature_url = "https://github.com/cenvero/fleet/releases/download/${version}/${archiveName}.minisig"
        sha256        = $null
      }
    } else {
      throw "Target not published yet: $target"
    }
  }

  $archivePath = Join-Path $tmp "fleet.zip"
  Invoke-WebRequest -Uri $binary.url -OutFile $archivePath

  if ($binary.signature_url) {
    $sigPath = Join-Path $tmp "fleet.minisig"
    Invoke-WebRequest -Uri $binary.signature_url -OutFile $sigPath
    $minisign = Get-Command minisign -ErrorAction SilentlyContinue
    if ($minisign) {
      if ($MinisignPublicKey -eq "REPLACE_WITH_MINISIGN_PUBLIC_KEY") {
        throw "Installer public key placeholder has not been replaced yet."
      }
      & $minisign.Source -Vm $archivePath -P $MinisignPublicKey -x $sigPath | Out-Null
      if ($LASTEXITCODE -ne 0) {
        throw "Minisign verification failed"
      }
    }
    else {
      Write-Warning "minisign is not installed; signature verification skipped."
    }
  }

  if ($binary.sha256) {
    $actual = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash.ToLowerInvariant()
    if ($actual -ne $binary.sha256.ToLowerInvariant()) {
      throw "Checksum mismatch"
    }
  }

  Expand-Archive -Path $archivePath -DestinationPath $tmp -Force

  $installDir = Join-Path $HOME ".local\bin"
  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  $source = Get-ChildItem -Path $tmp -Recurse -Filter "fleet.exe" | Select-Object -First 1
  if (-not $source) {
    throw "fleet.exe not found in archive"
  }

  Copy-Item $source.FullName (Join-Path $installDir "fleet.exe") -Force

  Write-Host "Installed Cenvero Fleet $version to $installDir\fleet.exe"
  Write-Host "Run: fleet init"
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

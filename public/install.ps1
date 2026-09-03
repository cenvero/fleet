$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$BaseUrl = "https://fleet.cenvero.org"
$Channel = if ($env:FLEET_CHANNEL) { $env:FLEET_CHANNEL } else { "stable" }
$VersionOverride = $env:FLEET_VERSION
$MinisignPublicKey = "RWRb53p9WTsWCO2RZT3bvjrZw4QjXnIo2R7NUqhPsfvhR8u0sS55hZb3"
$ApprovedHosts = @("fleet.cenvero.org", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com")

# A clean Windows installation does not include minisign. Bootstrap the official
# verifier from an immutable release URL and authenticate the archive before
# executing it. Update the URL, size, and digest together when upgrading.
$MinisignBootstrapUrl = "https://github.com/jedisct1/minisign/releases/download/0.12/minisign-0.12-win64.zip"
$MinisignBootstrapSize = 252505L
$MinisignBootstrapSha256 = "37b600344e20c19314b2e82813db2bfdcc408b77b876f7727889dbd46d539479"

# Windows PowerShell 5.1 does not reliably load these assemblies on first use.
# Load them before any HttpClient or ZipFile type is resolved.
Add-Type -AssemblyName System.Net.Http -ErrorAction Stop
Add-Type -AssemblyName System.IO.Compression.FileSystem -ErrorAction Stop

function Test-ForbiddenAddress([System.Net.IPAddress] $Address) {
  if ($Address.IsIPv4MappedToIPv6) { $Address = $Address.MapToIPv4() }
  if ([System.Net.IPAddress]::IsLoopback($Address) -or $Address.Equals([System.Net.IPAddress]::Any) -or $Address.Equals([System.Net.IPAddress]::IPv6Any)) { return $true }
  $bytes = $Address.GetAddressBytes()
  if ($Address.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetwork) {
    return $bytes[0] -eq 0 -or $bytes[0] -eq 10 -or $bytes[0] -eq 127 -or $bytes[0] -ge 224 -or
      ($bytes[0] -eq 100 -and $bytes[1] -ge 64 -and $bytes[1] -le 127) -or
      ($bytes[0] -eq 100 -and $bytes[1] -eq 100 -and $bytes[2] -eq 100 -and $bytes[3] -eq 200) -or
      ($bytes[0] -eq 169 -and $bytes[1] -eq 254) -or
      ($bytes[0] -eq 172 -and $bytes[1] -ge 16 -and $bytes[1] -le 31) -or
      ($bytes[0] -eq 192 -and $bytes[1] -eq 168) -or
      ($bytes[0] -eq 198 -and ($bytes[1] -eq 18 -or $bytes[1] -eq 19))
  }
  # fc00::/7, fe80::/10, and ff00::/8.
  return (($bytes[0] -band 0xFE) -eq 0xFC) -or ($bytes[0] -eq 0xFE -and ($bytes[1] -band 0xC0) -eq 0x80) -or $bytes[0] -eq 0xFF
}

function Assert-ApprovedUri([Uri] $Uri) {
  if (-not $Uri.IsAbsoluteUri -or $Uri.Scheme -cne "https" -or $ApprovedHosts -cnotcontains $Uri.DnsSafeHost.ToLowerInvariant()) {
    throw "Refusing unapproved download URL: $Uri"
  }
  if (-not [string]::IsNullOrEmpty($Uri.UserInfo) -or -not $Uri.IsDefaultPort) {
    throw "Refusing credentialed or non-default-port download URL: $Uri"
  }
  $addresses = [System.Net.Dns]::GetHostAddresses($Uri.DnsSafeHost)
  if (-not $addresses -or $addresses.Count -eq 0) { throw "Approved host did not resolve: $($Uri.DnsSafeHost)" }
  foreach ($address in $addresses) {
    if (Test-ForbiddenAddress $address) { throw "Download host $($Uri.DnsSafeHost) resolved to forbidden address $address" }
  }
}

function Get-ApprovedFile([string] $Url, [string] $OutFile) {
  $handler = [System.Net.Http.HttpClientHandler]::new()
  $handler.AllowAutoRedirect = $false
  $handler.UseProxy = $false
  $client = [System.Net.Http.HttpClient]::new($handler)
  $client.Timeout = [TimeSpan]::FromSeconds(30)
  try {
    $uri = [Uri]$Url
    for ($hop = 0; $hop -le 5; $hop++) {
      # Validate every Location, including all resolved addresses, before HttpClient
      # is allowed to issue the request for that hop.
      Assert-ApprovedUri $uri
      $response = $client.GetAsync($uri, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
      try {
        if ($response.StatusCode -eq [System.Net.HttpStatusCode]::OK) {
          $stream = [System.IO.File]::Create($OutFile)
          try { [void]$response.Content.CopyToAsync($stream).GetAwaiter().GetResult() } finally { $stream.Dispose() }
          return
        }
        if ([int]$response.StatusCode -notin @(301, 302, 303, 307, 308)) { throw "Unexpected download HTTP status: $($response.StatusCode)" }
        if ($hop -eq 5) { throw "Too many download redirects." }
        $location = $response.Headers.Location
        if (-not $location) { throw "Redirect missing Location header." }
        $uri = if ($location.IsAbsoluteUri) { $location } else { [Uri]::new($uri, $location) }
      }
      finally { $response.Dispose() }
    }
  }
  catch { Remove-Item -Force $OutFile -ErrorAction SilentlyContinue; throw }
  finally { $client.Dispose(); $handler.Dispose() }
}

function Resolve-Minisign([string] $TempDirectory, [string] $Architecture) {
  $installed = Get-Command -Name "minisign.exe", "minisign" -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($installed) { return $installed.Source }

  Write-Host "minisign was not found; downloading the pinned official verifier."
  $bootstrapArchive = Join-Path $TempDirectory "minisign-bootstrap.zip"
  Get-ApprovedFile $MinisignBootstrapUrl $bootstrapArchive
  if ((Get-Item $bootstrapArchive).Length -ne $MinisignBootstrapSize) {
    throw "Minisign bootstrap archive size mismatch."
  }
  $bootstrapHash = (Get-FileHash -Algorithm SHA256 -Path $bootstrapArchive).Hash.ToLowerInvariant()
  if ($bootstrapHash -cne $MinisignBootstrapSha256) {
    throw "Minisign bootstrap archive checksum mismatch."
  }

  $entryName = if ($Architecture -eq "arm64") {
    "minisign-win64/aarch64/minisign.exe"
  } else {
    "minisign-win64/x86_64/minisign.exe"
  }
  $bootstrapZip = [System.IO.Compression.ZipFile]::OpenRead($bootstrapArchive)
  try {
    $entries = @($bootstrapZip.Entries | Where-Object { $_.FullName -ceq $entryName -and $_.Name -ceq "minisign.exe" })
    if ($entries.Count -ne 1) { throw "Minisign bootstrap archive is missing the expected executable." }
    $bootstrapExecutable = Join-Path $TempDirectory "minisign.exe"
    $input = $entries[0].Open()
    $output = [System.IO.File]::Create($bootstrapExecutable)
    try { $input.CopyTo($output) } finally { $output.Dispose(); $input.Dispose() }
  }
  finally { $bootstrapZip.Dispose() }
  if ((Get-Item $bootstrapExecutable).Length -le 0) { throw "Minisign bootstrap executable is empty." }
  return $bootstrapExecutable
}

$archMap = @{ "AMD64" = "amd64"; "ARM64" = "arm64" }
# PROCESSOR_ARCHITEW6432 reports the native architecture when a 32-bit or x64
# PowerShell process is running under Windows-on-Windows emulation.
$processorArchitecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$processorArchitecture = ([string]$processorArchitecture).ToUpperInvariant()
$arch = $archMap[$processorArchitecture]
if (-not $arch) { throw "Unsupported architecture: $processorArchitecture" }
$target = "windows-$arch"

if ($MinisignPublicKey -eq "REPLACE_WITH_MINISIGN_PUBLIC_KEY") { throw "Installer public key is not configured." }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("fleet-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $minisignPath = Resolve-Minisign $tmp $arch
  $manifestPath = Join-Path $tmp "manifest.json"
  Get-ApprovedFile "$BaseUrl/manifest.json" $manifestPath
  $manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json

  $version = if ($VersionOverride) { $VersionOverride } else { $manifest.channels.$Channel.version }
  if (-not $version) { throw "Channel not found: $Channel" }
  if ($version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$') { throw "Invalid release version: $version" }

  $binary = $manifest.binaries.$version.$target
  if (-not $binary) { throw "Target not published: $version $target" }
  if (-not $binary.url) { throw "Release archive URL is required." }
  if (-not $binary.signature_url) { throw "Release signature URL is required." }
  if (-not $binary.sha256 -or $binary.sha256 -notmatch '^[0-9a-fA-F]{64}$') { throw "Valid SHA-256 checksum is required." }
  $artifactSize = 0L
  if (-not [Int64]::TryParse([string]$binary.size, [ref]$artifactSize) -or $artifactSize -le 0 -or $artifactSize -gt 1073741824) { throw "Valid artifact size is required." }

  $versionNoV = $version.Substring(1)
  $expectedUrl = "https://github.com/cenvero/fleet/releases/download/$version/fleet_${versionNoV}_windows_${arch}.zip"
  if ($binary.url -cne $expectedUrl) { throw "Manifest URL does not match product, version, and target." }
  if ($binary.signature_url -cne "$expectedUrl.minisig") { throw "Manifest signature URL does not match archive URL." }

  $archivePath = Join-Path $tmp "fleet.zip"
  $sigPath = Join-Path $tmp "fleet.minisig"
  Get-ApprovedFile $binary.url $archivePath
  if ((Get-Item $archivePath).Length -ne $artifactSize) { throw "Artifact size mismatch." }
  Get-ApprovedFile $binary.signature_url $sigPath

  & $minisignPath -Vm $archivePath -P $MinisignPublicKey -x $sigPath | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "Minisign verification failed." }
  $signatureLines = Get-Content $sigPath
  if ($signatureLines.Count -lt 3 -or -not $signatureLines[2].StartsWith("trusted comment: ")) { throw "Minisign trusted comment is missing." }
  $trustedComment = $signatureLines[2].Substring("trusted comment: ".Length)
  $expectedComment = "cenvero-fleet fleet $version $target"
  if ($trustedComment -cne $expectedComment) { throw "Signature is not bound to $version $target." }

  $actual = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash.ToLowerInvariant()
  if ($actual -cne $binary.sha256.ToLowerInvariant()) { throw "Checksum mismatch." }

  $zip = [System.IO.Compression.ZipFile]::OpenRead($archivePath)
  try {
    $entries = @($zip.Entries | Where-Object { $_.FullName -ceq "fleet.exe" -and $_.Name -ceq "fleet.exe" })
    if ($entries.Count -ne 1) { throw "Archive must contain exactly one canonical fleet.exe binary." }
    $sourcePath = Join-Path $tmp "fleet.exe"
    $input = $entries[0].Open()
    $output = [System.IO.File]::Create($sourcePath)
    try { $input.CopyTo($output) } finally { $output.Dispose(); $input.Dispose() }
  }
  finally { $zip.Dispose() }
  if ((Get-Item $sourcePath).Length -le 0) { throw "Canonical fleet.exe binary is empty." }

  $installDir = Join-Path $HOME ".local\bin"
  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  Copy-Item $sourcePath (Join-Path $installDir "fleet.exe") -Force
  Write-Host "Installed Cenvero Fleet $version to $installDir\fleet.exe"

  $currentPath = [Environment]::GetEnvironmentVariable("PATH", "User")
  $normalizedInstallDir = $installDir.TrimEnd("\")
  $pathEntries = @($currentPath -split ";" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | ForEach-Object { $_.Trim().TrimEnd("\") })
  if ($pathEntries -notcontains $normalizedInstallDir) {
    try {
      $newPath = @($currentPath, $installDir) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
      [Environment]::SetEnvironmentVariable("PATH", ($newPath -join ";"), "User")
      Write-Host "Added $installDir to your user PATH."
    }
    catch {
      Write-Warning "Fleet was installed, but the user PATH could not be updated: $($_.Exception.Message)"
      Write-Host "Add this directory to your user PATH manually: $installDir"
    }
  }

  # Make fleet available immediately in the PowerShell process that invoked the
  # installer, even before a new terminal inherits the updated user PATH.
  $processPathEntries = @($env:PATH -split ";" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | ForEach-Object { $_.Trim().TrimEnd("\") })
  if ($processPathEntries -notcontains $normalizedInstallDir) {
    $env:PATH = (@($env:PATH, $installDir) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }) -join ";"
  }
  Write-Host "Run: fleet init"
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

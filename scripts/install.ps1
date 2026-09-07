# install.ps1 — one-line installer for pgArchiMigrator's portable
# Windows package (see scripts/build-artifact.sh's own "portable" mode
# and deploy/platforms/windows-x64.json for exactly what this
# downloads). See install.sh's own header comment for the Linux/macOS
# equivalent and the full reasoning behind every step here — this
# mirrors it exactly, just in PowerShell.
#
# Usage:
#   irm https://raw.githubusercontent.com/pgarchihub/pgarchimigrator/main/scripts/install.ps1 | iex
#
# Override the version (default: latest GitHub release):
#   $env:PGARCHIMIGRATOR_VERSION = "v2.0.0"
#   irm .../install.ps1 | iex

$ErrorActionPreference = "Stop"

$Repo = "pgarchihub/pgarchimigrator"

# --- 1. Architecture — deploy/platforms/ only publishes windows-x64.json
#     today; a 32-bit or ARM64 Windows machine has no matching package.
$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -ne "AMD64") {
    Write-Error "unsupported architecture: $arch (only windows-x64 is currently published — see deploy/platforms/)"
    exit 1
}
Write-Host "Detected platform: windows-x64"

# --- 2. Resolve the version and download URL.
$version = $env:PGARCHIMIGRATOR_VERSION
if (-not $version) {
    Write-Host "Resolving the latest release..."
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    $version = $release.tag_name
    if (-not $version) {
        Write-Error "could not resolve the latest release version — set `$env:PGARCHIMIGRATOR_VERSION explicitly"
        exit 1
    }
}
Write-Host "Installing pgArchiMigrator $version"

$archiveName = "pgarchimigrator-$version-windows-x64.zip"
$releaseBase = "https://github.com/$Repo/releases/download/$version"

$tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmpDir | Out-Null
try {
    $archivePath = Join-Path $tmpDir $archiveName
    Write-Host "Downloading $archiveName..."
    try {
        Invoke-WebRequest -Uri "$releaseBase/$archiveName" -OutFile $archivePath
    } catch {
        Write-Error "failed to download $archiveName - check that $version was actually released for windows-x64"
        exit 1
    }

    $checksumsPath = Join-Path $tmpDir "checksums.sha256"
    Write-Host "Downloading checksums.sha256..."
    try {
        Invoke-WebRequest -Uri "$releaseBase/checksums.sha256" -OutFile $checksumsPath
    } catch {
        Write-Error "failed to download checksums.sha256"
        exit 1
    }

    # --- 3. Verify the download's integrity — see install.sh's own
    #     header comment for exactly what this does and does not
    #     guarantee (transport + checksum only, not the Ed25519
    #     artifact-descriptor signature — that remains a separate,
    #     optional manual step printed at the end).
    Write-Host "Verifying checksum..."
    $expectedLine = Get-Content $checksumsPath | Where-Object { $_ -match [regex]::Escape($archiveName) }
    if (-not $expectedLine) {
        Write-Error "no checksum entry found for $archiveName in checksums.sha256"
        exit 1
    }
    $expectedHash = ($expectedLine -split '\s+')[0]
    $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash
    if ($actualHash.ToLower() -ne $expectedHash.ToLower()) {
        Write-Error "CHECKSUM MISMATCH - the downloaded archive does not match checksums.sha256. Do not run this build; try again or report this."
        exit 1
    }
    Write-Host "Checksum OK."

    # --- 4. Extract into this project's own standard install directory
    #     — matching internal/deploylayout's own DefaultEcosystemRoot()
    #     + ProductPathSegments exactly (%LOCALAPPDATA%\ArchiOrbitLabs\
    #     Platforms\PostgreSQL\pgArchiMigrator), so a script install
    #     lands in the SAME place a manual unpack would.
    $installDir = Join-Path $env:LOCALAPPDATA "ArchiOrbitLabs\Platforms\PostgreSQL\pgArchiMigrator"
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null

    Write-Host "Extracting to $installDir..."
    $extractDir = Join-Path $tmpDir "extracted"
    Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force

    # build-artifact.sh's own portable-package layout places the binary
    # at the archive's own top level, named per deploy/platforms/
    # windows-x64.json's own binaryName field ("pgarchimigrator.exe") —
    # copy just the binary itself into installDir rather than the whole
    # extracted tree, so a re-run of this script doesn't accumulate old
    # package metadata.
    $binaryPath = Get-ChildItem -Path $extractDir -Filter "pgarchimigrator.exe" -Recurse -Depth 1 | Select-Object -First 1
    if (-not $binaryPath) {
        Write-Error "extraction succeeded but pgarchimigrator.exe wasn't found where expected - the portable package's own layout may have changed"
        exit 1
    }
    Copy-Item -Path $binaryPath.FullName -Destination (Join-Path $installDir "pgarchimigrator.exe") -Force

    Write-Host ""
    Write-Host "pgArchiMigrator $version installed to:"
    Write-Host "  $installDir\pgarchimigrator.exe"
    Write-Host ""
    Write-Host "Add it to your PATH for this session (this script never edits your permanent PATH for you):"
    Write-Host "  `$env:PATH = `"$installDir;`$env:PATH`""
    Write-Host ""
    Write-Host "To add it permanently, use Windows' own 'Edit environment variables for your account' settings, or:"
    Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `"$installDir;`" + [Environment]::GetEnvironmentVariable('PATH', 'User'), 'User')"
    Write-Host ""
    Write-Host "For the stronger integrity guarantee beyond the checksum check above"
    Write-Host "(that this artifact was genuinely built and signed by this project,"
    Write-Host "not just whatever bytes were at this URL), verify its Ed25519"
    Write-Host "artifact descriptor separately:"
    Write-Host "  Invoke-WebRequest -Uri `"$releaseBase/$archiveName.descriptor.json`" -OutFile pgarchimigrator.descriptor.json"
    Write-Host "  pgarchisign verify --descriptor pgarchimigrator.descriptor.json --pubkey <the project's published public key>"
    Write-Host ""
    Write-Host "Get started:"
    Write-Host "  & `"$installDir\pgarchimigrator.exe`" --help"
} finally {
    Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}

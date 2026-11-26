param([switch]$SkipCompile, [string]$Version)
$ErrorActionPreference = 'Stop'
$repository = Split-Path $PSScriptRoot
Set-Location -LiteralPath $repository
function Assert-Success([string]$Step) { if ($LASTEXITCODE -ne 0) { throw "$Step failed (exit $LASTEXITCODE)" } }
# Defence in depth: the service has a hook for its tests (core/vpn-engine, Cargo feature test-hooks, which
# only the daemon's dev-dependencies turn on) that replaces the pinned fetch of dynamic keys by a plain
# local one. A release build never enables it; nothing that ships may even contain the name of its variable.
function Assert-NoTestHook([string[]]$Files) {
    $needle = 'GHOSTLINK_TEST_DYNAMIC_KEY_URL'
    $latin1 = [System.Text.Encoding]::GetEncoding(28591) # one char per byte, so IndexOf finds the bytes of the name
    foreach ($file in $Files) {
        if ($latin1.GetString([System.IO.File]::ReadAllBytes($file)).IndexOf($needle, [System.StringComparison]::Ordinal) -ge 0) {
            throw "$file contains ${needle}: it was built with the test-hooks feature and must not be released"
        }
    }
}
if (-not $Version) {
    # Same authoritative version and consistency check as the release workflow.
    $Version = (& node (Join-Path $PSScriptRoot 'release-version.mjs')).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $Version) { throw 'Version declarations disagree; run node scripts/release-version.mjs' }
}
# Windows Installer takes a numeric a.b.c product version (the first two fields up to 255, the third
# up to 65535), not a semantic version with a pre-release such as 0.9.1-rc.1. The installer carries
# the numeric core; the file names and everything else keep the whole version.
if ($Version -match '^(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$') {
    if ([int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) { throw "Version '$Version' is too large for a Windows Installer product version (255.255.65535 at most)" }
    $msiVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3])"
} else { throw "Version '$Version' is not a semantic version" }
if (-not $SkipCompile) {
    $env:RUSTFLAGS = '-C target-feature=+crt-static'
    & "$PSScriptRoot\cargo.ps1" build --release --locked --workspace
    Assert-Success 'Rust release build'
    & "$PSScriptRoot\cargo.ps1" build --release --locked --manifest-path apps/desktop/src-tauri/Cargo.toml
    Assert-Success 'Desktop release build'
    # CGO_ENABLED=0 as in CI's tests and the published BUILD.txt; with a C compiler on PATH, Go
    # would otherwise build the worker with cgo.
    $previousCgo = $env:CGO_ENABLED
    $env:CGO_ENABLED = '0'
    Push-Location core/outline-worker
    try { & go build -mod=readonly -trimpath -buildvcs=false -tags with_gvisor,with_utls,with_wireguard -ldflags '-s -w' -o '..\..\target\release\ghostlink-tunnel.exe' .; Assert-Success 'Managed tunnel worker build' }
    finally { Pop-Location; $env:CGO_ENABLED = $previousCgo }
}
$stage = Join-Path $repository 'artifacts\windows-x64\Ghostlink'
# Start from an empty stage: a file an earlier build left there (a license that no longer ships, say)
# would go into the portable archive and, harvested from this folder, into the installer.
if (Test-Path -LiteralPath $stage) {
    if ((Get-Item -LiteralPath $stage -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Refusing to clear a linked staging directory' }
    Remove-Item -LiteralPath $stage -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $stage,(Join-Path $stage 'engine'),(Join-Path $stage 'licenses') | Out-Null
foreach ($name in @('ghostlink.exe','ghostlinkd.exe','ghostlink-mcp.exe')) { Copy-Item -LiteralPath (Join-Path $repository "target\release\$name") -Destination (Join-Path $stage $name) }
Copy-Item -LiteralPath (Join-Path $repository 'target\release\ghostlink-tunnel.exe') -Destination (Join-Path $stage 'engine\ghostlink-tunnel.exe')
Copy-Item -LiteralPath (Join-Path $repository 'apps\desktop\src-tauri\target\release\ghostlink-desktop.exe') -Destination (Join-Path $stage 'ghostlink-desktop.exe')
# Every program that goes into the installer and the portable archive, as staged (also with -SkipCompile).
Assert-NoTestHook @(Get-ChildItem -LiteralPath $stage -Recurse -Filter '*.exe' | ForEach-Object FullName)
Copy-Item -LiteralPath (Join-Path $repository 'LICENSE'),(Join-Path $repository 'NOTICE.md') -Destination (Join-Path $stage 'licenses')
Copy-Item -Path (Join-Path $repository 'LICENSES\*') -Destination (Join-Path $stage 'licenses') -Recurse -Force
$wixDirectory = Join-Path $repository '.tools\wix314'
if (-not (Test-Path -LiteralPath (Join-Path $wixDirectory 'candle.exe'))) { throw 'Run scripts/bootstrap-windows.ps1 to obtain the pinned WiX tools.' }
$harvest = Join-Path $repository 'artifacts\windows-x64\licenses.wxs'
& (Join-Path $wixDirectory 'heat.exe') dir (Join-Path $stage 'licenses') -cg LicenseFiles -dr LicensesFolder -ag -sfrag -srd -var var.Licenses -out $harvest
Assert-Success 'License harvest'
$objectDirectory = Join-Path $repository 'artifacts\windows-x64\wix'
New-Item -ItemType Directory -Force -Path $objectDirectory | Out-Null
# One explicit output file per source: a directory -out needs a trailing backslash, which
# Windows PowerShell 5.1 turns into an escaped quote when the path contains a space.
# WixUtilExtension (in the pinned WiX tools: scripts/bootstrap-windows.ps1) is for util:CloseApplication
# and util:ServiceConfig.
$candleArguments = @('-nologo', '-arch', 'x64', '-ext', 'WixUtilExtension', "-dVersion=$Version", "-dMsiVersion=$msiVersion", "-dStage=$stage", "-dRoot=$repository", "-dLicenses=$(Join-Path $stage 'licenses')")
& (Join-Path $wixDirectory 'candle.exe') @candleArguments -out (Join-Path $objectDirectory 'Ghostlink.wixobj') (Join-Path $repository 'packages\windows\Ghostlink.wxs')
Assert-Success 'Installer compilation'
& (Join-Path $wixDirectory 'candle.exe') @candleArguments -out (Join-Path $objectDirectory 'licenses.wixobj') $harvest
Assert-Success 'License fragment compilation'
$installer = Join-Path $repository "artifacts\windows-x64\Ghostlink-$Version-windows-x64.msi"
# ICE61 and ICE63 are warnings about two things that are meant (see the comments in Ghostlink.wxs: the
# same-version upgrade and the close of the desktop app ahead of RemoveExistingProducts). Every other
# ICE still runs; a new warning shows.
& (Join-Path $wixDirectory 'light.exe') -nologo -ext WixUtilExtension -sice:ICE61 -sice:ICE63 -out $installer (Join-Path $objectDirectory 'Ghostlink.wixobj') (Join-Path $objectDirectory 'licenses.wixobj')
Assert-Success 'Installer linking and validation'
Compress-Archive -Path $stage -DestinationPath (Join-Path $repository "artifacts\windows-x64\Ghostlink-$Version-windows-x64-portable.zip") -Force
Get-FileHash -LiteralPath $installer -Algorithm SHA256 | Format-List
Write-Output 'Installer compiled. Installation/elevation and real TUN routing still require an authorized Windows test.'

<#
  Installs the Ghostlink MSI built by this run, for real, on a throwaway Windows runner (an elevated
  administrator, thrown away after the job; never run it on a machine you use). This is the first
  place the installed service runs as SYSTEM for another account, which nothing else reaches.

  1. A fresh install: the service exists, runs as LocalSystem for the installing user, restarts after
     a crash (sc qfailure), and the installed CLI, run as that user, gets through the service's
     narrowed pipe rights (status and doctor). The same user cannot start a second server on the
     pipe's name.
  2. Uninstall: the service and the program files are gone.
  3. The upgrade from the published 0.9.1 while a program named ghostlink-desktop.exe runs from the
     installed folder (a stand-in: a copy of ping.exe): it is ended, no reboot is asked for, one
     product is left, the owner is kept and the service runs again.
  4. A build of the same version (a release candidate, -SameVersionMsi) replaced by the release.
  5. Uninstall again.

  The msiexec logs go to -LogDirectory (the workflow uploads them when the step fails).
  Usage: pwsh tests/packages/msi-install.ps1 -Msi FILE -Version X.Y.Z[-rc.N] -LogDirectory DIR [-SameVersionMsi FILE]
#>
param(
    [Parameter(Mandatory)] [string] $Msi,
    [Parameter(Mandatory)] [string] $Version,
    [Parameter(Mandatory)] [string] $LogDirectory,
    [string] $SameVersionMsi
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Windows PowerShell 5.1 downloads 10 times slower with a progress bar

# The release the upgrade starts from: published, so its file never changes (the hash is that of
# SHA256SUMS.txt of the release, and of the download). Change both for another release.
$previousUrl = 'https://github.com/winyannainghtut/ghostlink/releases/download/v0.9.1/Ghostlink-0.9.1-windows-x64.msi'
$previousSha256 = '85D6E5081A85AE2FE5BF26B24E20760D8D17F78C1472B5BE90E74FA37B677C21'
$previousVersion = [version]'0.9.1'

$programFiles = Join-Path $env:ProgramFiles 'Ghostlink'
$cli = Join-Path $programFiles 'ghostlink.exe'
$uninstallKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall'

function Write-Step([string]$Text) { Write-Host ''; Write-Host "=== $Text" }

# msiexec with a log; returns its exit code. Quoting is by hand: Start-Process joins an argument list
# with spaces, and the paths here may hold some.
function Invoke-Msiexec([string]$Verb, [string]$Package, [string]$LogName) {
    $log = Join-Path $LogDirectory $LogName
    $process = Start-Process -FilePath 'msiexec.exe' -ArgumentList "$Verb `"$Package`" /qn /norestart /l*v `"$log`"" -Wait -PassThru
    return $process.ExitCode
}

# The lines of an msiexec log around what failed, to read in the job's output.
function Show-MsiError([string]$LogName) {
    $log = Join-Path $LogDirectory $LogName
    if (Test-Path -LiteralPath $log) {
        Write-Host "--- ${LogName}: around the failure"
        Get-Content -LiteralPath $log | Select-String -Pattern 'Return value 3|Error \d{4}|Custom Action .* returned|CustomAction .* returned actual error|failed' -Context 4, 2 | Select-Object -First 12 | ForEach-Object { Write-Host $_ }
    }
}

# Fails unless msiexec ends with 0: 3010 (a reboot is needed) is a failure too, because files in use
# are what the installer is there to deal with.
function Install-Product([string]$Package, [string]$LogName) {
    $code = Invoke-Msiexec '/i' $Package $LogName
    if ($code -ne 0) { Show-MsiError $LogName; throw "msiexec /i $Package ended with $code (see $LogName)" }
}
function Uninstall-Product([string]$Package, [string]$LogName) {
    $code = Invoke-Msiexec '/x' $Package $LogName
    if ($code -ne 0) { Show-MsiError $LogName; throw "msiexec /x $Package ended with $code (see $LogName)" }
}

function Get-GhostlinkService { return Get-CimInstance -ClassName Win32_Service -Filter "Name='Ghostlink'" }

# The installed service: running as LocalSystem, started by itself at boot, for this owner.
function Assert-ServiceRuns([string]$OwnerSid) {
    $service = $null
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        $service = Get-GhostlinkService
        if ($service -and $service.State -eq 'Running') { break }
        Start-Sleep -Seconds 1
    }
    if (-not $service) { throw 'the Ghostlink service does not exist' }
    Write-Host "service: $($service.State), $($service.StartMode), $($service.StartName), $($service.PathName)"
    if ($service.State -ne 'Running') { throw "the Ghostlink service is $($service.State), not Running" }
    if ($service.StartName -ne 'LocalSystem') { throw "the Ghostlink service runs as $($service.StartName), not LocalSystem" }
    if ($service.StartMode -ne 'Auto') { throw "the Ghostlink service starts $($service.StartMode), not Auto" }
    if ($service.PathName -notlike "*--owner-sid `"$OwnerSid`"*") { throw "the service's command line does not name the owner $OwnerSid" }
    $saved = (Get-ItemProperty -Path 'HKLM:\SOFTWARE\Ghostlink' -Name OwnerSID).OwnerSID
    if ($saved -ne $OwnerSid) { throw "the saved owner is $saved, not $OwnerSid" }
}

# What `sc qfailure` says about a service that restarts twice after a crash and is then left
# alone: the number of restarts, their delay (ms) and the reset period (s).
function ConvertFrom-FailureActions([string[]]$Output) {
    $text = $Output -join "`n"
    $reset = [regex]::Match($text, 'RESET_PERIOD[^:]*:\s*(\d+)')
    $restarts = [regex]::Matches($text, 'RESTART -- Delay = (\d+) milliseconds')
    return [pscustomobject]@{
        ResetSeconds = if ($reset.Success) { [int]$reset.Groups[1].Value } else { $null }
        Restarts = @($restarts | ForEach-Object { [int]$_.Groups[1].Value })
        Other = [bool]([regex]::IsMatch($text, 'REBOOT -- Delay|RUN PROCESS -- Delay'))
    }
}
function Assert-RestartsAfterACrash {
    $output = & sc.exe qfailure Ghostlink
    if ($LASTEXITCODE -ne 0) { throw "sc qfailure Ghostlink failed: $($output -join ' ')" }
    Write-Host ($output -join "`n")
    $actions = ConvertFrom-FailureActions $output
    if ($actions.ResetSeconds -ne 86400) { throw "the failure count resets after $($actions.ResetSeconds) s, not one day" }
    if (($actions.Restarts -join ',') -ne '10000,10000') { throw "the restarts are '$($actions.Restarts -join ',')' ms, not two of 10000" }
    if ($actions.Other) { throw 'the service reboots the machine or runs a program after a crash' }
}

# Runs the installed CLI as this user, once or more (the service may be a moment from ready).
function Invoke-Ghostlink([string[]]$Arguments) {
    # Windows PowerShell 5.1 turns a native program's stderr into a terminating error under 'Stop'.
    $ErrorActionPreference = 'Continue'
    $last = ''
    for ($attempt = 0; $attempt -lt 20; $attempt++) {
        $output = & $cli --json @Arguments 2>&1
        if ($LASTEXITCODE -eq 0) { return ($output -join "`n" | ConvertFrom-Json) }
        $last = $output -join "`n"
        Start-Sleep -Seconds 1
    }
    throw "ghostlink --json $($Arguments -join ' ') does not answer for this user: $last"
}
function Assert-CliGetsIn([string]$ExpectedVersion) {
    $reported = (& $cli --version) -join ' '
    Write-Host "ghostlink --version: $reported"
    if ($reported -notlike "*$ExpectedVersion*") { throw "the installed ghostlink is '$reported', not $ExpectedVersion" }
    $status = Invoke-Ghostlink @('status')
    Write-Host "status: $($status | ConvertTo-Json -Compress -Depth 5)"
    if (-not $status.success -or $status.data.status -ne 'disconnected') { throw 'ghostlink status is not "disconnected"' }
    $doctor = Invoke-Ghostlink @('doctor')
    Write-Host "doctor: $($doctor | ConvertTo-Json -Compress -Depth 5)"
    if (-not $doctor.success -or $doctor.data.outline_engine_available -ne $true) { throw 'ghostlink doctor does not find the tunnel worker the service was installed with' }
}

# The owner holds the narrow rights of a SYSTEM service's pipe: read and write, but not the right
# to create another instance. A second server on the pipe's name, which a client would take for
# the service's (it only checks who made it), is therefore refused. The control shows that two
# instances with these arguments can be made on a pipe of this process's own; the arguments are
# those of the service's pipe (byte mode, duplex, 64 instances), as a mismatch is refused as well.
function Assert-NoSecondServer([string]$Pipe = 'ghostlink') {
    $direction = [System.IO.Pipes.PipeDirection]::InOut
    $mode = [System.IO.Pipes.PipeTransmissionMode]::Byte
    $options = [System.IO.Pipes.PipeOptions]::Asynchronous
    $own = "ghostlink-ci-control-$PID"
    $first = [System.IO.Pipes.NamedPipeServerStream]::new($own, $direction, 64, $mode, $options)
    try {
        $second = [System.IO.Pipes.NamedPipeServerStream]::new($own, $direction, 64, $mode, $options)
        $second.Dispose()
    } finally { $first.Dispose() }
    $verdict = 'started'
    try {
        $server = [System.IO.Pipes.NamedPipeServerStream]::new($Pipe, $direction, 64, $mode, $options)
        $server.Dispose()
    } catch [System.UnauthorizedAccessException] {
        $verdict = 'refused'
    } catch {
        $verdict = "inconclusive ($($_.Exception.GetType().Name): $($_.Exception.Message))"
    }
    Write-Host "a second server on the $Pipe pipe: $verdict"
    if ($verdict -eq 'started') { throw "this user started a second server on the $Pipe pipe" }
    if ($verdict -ne 'refused') { Write-Host "::warning::the check for a second pipe server was $verdict" }
}

# Nothing of Ghostlink is left: the service, its processes, the program folder, its PATH entry
# and the entry in Apps and features.
function Assert-Gone {
    if (Get-GhostlinkService) { throw 'the Ghostlink service is still there' }
    if (Get-Process -Name ghostlinkd, ghostlink-tunnel, ghostlink-desktop -ErrorAction SilentlyContinue) { throw 'a Ghostlink program is still running' }
    if (Test-Path -LiteralPath $programFiles) { throw "$programFiles is still there: $((Get-ChildItem -LiteralPath $programFiles -Recurse -Force | ForEach-Object FullName) -join ', ')" }
    if ([Environment]::GetEnvironmentVariable('Path', 'Machine') -like "*$programFiles*") { throw 'the machine PATH still names the program folder' }
    if (@(Get-Entries).Count -ne 0) { throw 'Apps and features still lists Ghostlink' }
}
function Get-Entries {
    return Get-ChildItem -Path $uninstallKey | ForEach-Object { Get-ItemProperty -Path $_.PSPath } | Where-Object { $_.DisplayName -eq 'Ghostlink' }
}

# The one product that is installed after an upgrade, for the full version in its comments.
function Assert-OneProduct([string]$FullVersion) {
    $entries = @(Get-Entries)
    if ($entries.Count -ne 1) { throw "Apps and features lists Ghostlink $($entries.Count) times, not once" }
    Write-Host "installed: $($entries[0].DisplayName) $($entries[0].DisplayVersion) ($($entries[0].Comments))"
    if ($entries[0].Comments -ne "Ghostlink $FullVersion") { throw "the installed product is '$($entries[0].Comments)', not 'Ghostlink $FullVersion'" }
}

function Assert-NoPendingRename {
    $pending = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager' -Name PendingFileRenameOperations -ErrorAction SilentlyContinue).PendingFileRenameOperations
    if ($pending -match 'Ghostlink') { throw "a file of Ghostlink waits for a reboot to be replaced or removed: $($pending -join ' ')" }
}

function Get-MsiVersion([string]$Package) {
    $installer = New-Object -ComObject WindowsInstaller.Installer
    $database = $installer.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $installer, @($Package, 0))
    $view = $database.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $database, @("SELECT Value FROM Property WHERE Property='ProductVersion'"))
    $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
    $record = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
    return [version]$record.GetType().InvokeMember('StringData', 'GetProperty', $null, $record, @(1))
}

function Save-Previous {
    $file = Join-Path $LogDirectory 'previous.msi'
    # Release downloads sometimes answer 5xx for a while: try a few times (a loop: Windows PowerShell 5.1
    # has no -MaximumRetryCount).
    for ($attempt = 1; ; $attempt++) {
        try { Invoke-WebRequest -Uri $previousUrl -OutFile $file -UseBasicParsing; break }
        catch {
            Remove-Item -LiteralPath $file -Force -ErrorAction SilentlyContinue
            if ($attempt -ge 5) { throw }
            Write-Warning "Downloading the previous release failed ($($_.Exception.Message)); trying again in 15 s"
            Start-Sleep -Seconds 15
        }
    }
    if ((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash -ne $previousSha256) { throw 'Checksum mismatch: the published previous MSI' }
    return $file
}

# --- the run ------------------------------------------------------------------------------------
$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'run this as an administrator, on a runner that is thrown away' }
$owner = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
New-Item -ItemType Directory -Force -Path $LogDirectory | Out-Null
$Msi = (Resolve-Path -LiteralPath $Msi).Path
Write-Host "owner (this user): $owner; installing $Msi"
if ((Get-GhostlinkService) -or (Test-Path -LiteralPath $programFiles) -or @(Get-Entries).Count) { throw 'Ghostlink is installed on this machine already: not a clean runner' }
$msiVersion = Get-MsiVersion $Msi
$standIn = $null
try {
    Write-Step "1. A fresh install of $Version, as this user ($owner)"
    Install-Product $Msi '1-install.log'
    Assert-ServiceRuns $owner
    Assert-RestartsAfterACrash
    Assert-CliGetsIn $Version
    Assert-NoSecondServer
    if ([Environment]::GetEnvironmentVariable('Path', 'Machine') -notlike "*$programFiles*") { throw 'the machine PATH does not name the program folder' }
    Assert-OneProduct $Version

    Write-Step '2. Uninstall'
    Uninstall-Product $Msi '2-uninstall.log'
    Assert-Gone

    if ($msiVersion -gt $previousVersion) {
        Write-Step "3. Upgrade from the published $previousVersion, with the desktop app running"
        $previous = Save-Previous
        Install-Product $previous '3-previous.log'
        Assert-ServiceRuns $owner
        # The app keeps running in the tray in real life. Its stand-in is a program of this name that
        # runs from the installed folder, so that the file is in use.
        $desktop = Join-Path $programFiles 'ghostlink-desktop.exe'
        Copy-Item -LiteralPath (Join-Path $env:SystemRoot 'System32\PING.EXE') -Destination $desktop -Force
        $standIn = Start-Process -FilePath $desktop -ArgumentList '-t', '127.0.0.1' -WindowStyle Hidden -PassThru
        Start-Sleep -Seconds 2
        if ($standIn.HasExited) { throw 'the stand-in for the desktop app did not stay running' }
        $held = $false
        try { [IO.File]::Open($desktop, 'Open', 'Write', 'None').Dispose() } catch [IO.IOException] { $held = $true }
        if (-not $held) { throw 'the stand-in does not hold its file, so the test would prove nothing' }
        Install-Product $Msi '3-upgrade.log'
        $standIn.Refresh()
        if (-not $standIn.WaitForExit(20000)) { throw 'the upgrade left the desktop app running' }
        Assert-NoPendingRename
        Assert-ServiceRuns $owner
        Assert-RestartsAfterACrash
        Assert-CliGetsIn $Version
        Assert-OneProduct $Version
        Uninstall-Product $Msi '3-uninstall.log'
        Assert-Gone
    } else {
        Write-Host "(no upgrade test: $msiVersion is not newer than the published $previousVersion)"
    }

    if ($SameVersionMsi) {
        Write-Step '4. A build of the same version replaced by the release'
        $SameVersionMsi = (Resolve-Path -LiteralPath $SameVersionMsi).Path
        if ((Get-MsiVersion $SameVersionMsi) -ne $msiVersion) { throw "$SameVersionMsi is not of the version of $Msi" }
        Install-Product $SameVersionMsi '4-same-version.log'
        Assert-ServiceRuns $owner
        Install-Product $Msi '4-release.log'
        Assert-ServiceRuns $owner
        Assert-CliGetsIn $Version
        Assert-OneProduct $Version
        Write-Step '5. Uninstall'
        Uninstall-Product $Msi '5-uninstall.log'
        Assert-Gone
    }
    Write-Host ''
    Write-Host 'The installer installs, runs as SYSTEM for this user, upgrades and uninstalls.'
} finally {
    if ($standIn -and -not $standIn.HasExited) { Stop-Process -Id $standIn.Id -Force -ErrorAction SilentlyContinue }
}
# A native program the checks tolerated for a try or two (the CLI before the service is ready) must not
# leave its exit code as the step's.
exit 0

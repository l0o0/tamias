$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true') { throw 'Run this installation smoke test only on an ephemeral GitHub runner.' }
$root = Split-Path -Parent $PSScriptRoot
$setup = Join-Path $root "dist\tamias-$env:VERSION-windows-$env:ARCH-setup.exe"
$install = Join-Path $env:RUNNER_TEMP 'tamias-installer-smoke'
$data = Join-Path $env:RUNNER_TEMP 'tamias-installer-data'
if (Test-Path $install) { throw "Refusing to overwrite an existing test install: $install" }
New-Item -ItemType Directory -Path $data -Force | Out-Null
Set-Content -LiteralPath (Join-Path $data 'keep.txt') -Value 'Preserve application data'

$windowProbeSource = @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public static class TamiasInstallerWindowProbe
{
    public sealed class WindowInfo
    {
        public long Handle { get; set; }
        public uint ProcessId { get; set; }
        public bool Visible { get; set; }
        public long Owner { get; set; }
        public string ClassName { get; set; }
        public string Title { get; set; }
    }

    [UnmanagedFunctionPointer(CallingConvention.Winapi)]
    private delegate bool EnumWindowsProc(IntPtr window, IntPtr parameter);

    [DllImport("user32.dll", SetLastError = true)]
    private static extern bool EnumWindows(EnumWindowsProc callback, IntPtr parameter);

    [DllImport("user32.dll", SetLastError = true)]
    private static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);

    [DllImport("user32.dll")]
    private static extern bool IsWindowVisible(IntPtr window);

    [DllImport("user32.dll")]
    private static extern IntPtr GetWindow(IntPtr window, uint command);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetWindowText(IntPtr window, StringBuilder text, int maxCount);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    private static extern int GetClassName(IntPtr window, StringBuilder className, int maxCount);

    public static WindowInfo[] GetForProcess(uint targetProcessId)
    {
        var windows = new List<WindowInfo>();
        EnumWindows(delegate(IntPtr window, IntPtr parameter)
        {
            uint processId;
            GetWindowThreadProcessId(window, out processId);
            if (processId == targetProcessId)
            {
                var title = new StringBuilder(512);
                var className = new StringBuilder(256);
                GetWindowText(window, title, title.Capacity);
                GetClassName(window, className, className.Capacity);
                windows.Add(new WindowInfo
                {
                    Handle = window.ToInt64(),
                    ProcessId = processId,
                    Visible = IsWindowVisible(window),
                    Owner = GetWindow(window, 4).ToInt64(), // GW_OWNER
                    ClassName = className.ToString(),
                    Title = title.ToString()
                });
            }
            return true;
        }, IntPtr.Zero);
        return windows.ToArray();
    }
}
'@
if (-not ('TamiasInstallerWindowProbe' -as [type])) {
    Add-Type -TypeDefinition $windowProbeSource -Language CSharp
}

function Write-LogTail([string] $Path, [string] $Label) {
    Write-Host "--- $Label ($Path) ---"
    if (Test-Path -LiteralPath $Path -PathType Leaf) {
        $lines = Get-Content -LiteralPath $Path -Tail 100 -ErrorAction SilentlyContinue
        if ($lines) { $lines | Write-Host } else { Write-Host '<empty>' }
    } else {
        Write-Host '<not created>'
    }
}

$log = Join-Path $env:RUNNER_TEMP 'tamias-installer.log'
$process = Start-Process -FilePath $setup -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', "/DIR=`"$install`"", "/LOG=`"$log`"") -PassThru
if (-not $process.WaitForExit(120000)) { $process.Kill(); throw 'Installer timed out' }
if ($process.ExitCode -ne 0) {
    Write-LogTail $log 'Installer log'
    throw "Installer exited with $($process.ExitCode)"
}
$exe = Join-Path $install 'tamias.exe'
foreach ($file in @('tamias.exe', 'LICENSE', 'THIRD_PARTY_NOTICES.txt', 'unins000.exe')) {
    if (-not (Test-Path (Join-Path $install $file))) { throw "Installed payload missing $file" }
}
$stdout = Join-Path $env:RUNNER_TEMP 'tamias-version.txt'
$versionStderr = Join-Path $env:RUNNER_TEMP 'tamias-version.stderr.txt'
$versionProcess = Start-Process -FilePath $exe -ArgumentList '-version' -RedirectStandardOutput $stdout -RedirectStandardError $versionStderr -PassThru
if (-not $versionProcess.WaitForExit(30000)) {
    $versionProcess.Kill()
    $versionProcess.WaitForExit()
    Write-LogTail $stdout 'Version probe stdout'
    Write-LogTail $versionStderr 'Version probe stderr'
    throw 'Installed version probe timed out after 30 seconds'
}
$versionOutput = if (Test-Path -LiteralPath $stdout) { ([string](Get-Content -LiteralPath $stdout -Raw)).Trim() } else { '' }
if ($versionProcess.ExitCode -ne 0 -or $versionOutput -ne $env:VERSION) {
    Write-LogTail $stdout 'Version probe stdout'
    Write-LogTail $versionStderr 'Version probe stderr'
    throw "Installed application version mismatch (exit=$($versionProcess.ExitCode), output='$versionOutput', expected='$env:VERSION')"
}

$appStdout = Join-Path $env:RUNNER_TEMP 'tamias-startup.stdout.txt'
$appStderr = Join-Path $env:RUNNER_TEMP 'tamias-startup.stderr.txt'
$app = $null
$startupFailure = $null
$window = $null
$windows = @()
$mainWindowHandleSnapshot = 'unavailable'
try {
    $app = Start-Process -FilePath $exe -ArgumentList @('-data-dir', "`"$data`"") -RedirectStandardOutput $appStdout -RedirectStandardError $appStderr -PassThru
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $app.Refresh()
        $windows = [TamiasInstallerWindowProbe]::GetForProcess([uint32] $app.Id)
        $window = $windows | Where-Object { $_.Visible -and $_.Title -eq '小花鼠' } | Select-Object -First 1
        if ($null -ne $window -or $app.HasExited) { break }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)

    $app.Refresh()
    $windows = [TamiasInstallerWindowProbe]::GetForProcess([uint32] $app.Id)
    $window = $windows | Where-Object { $_.Visible -and $_.Title -eq '小花鼠' } | Select-Object -First 1
    try { $mainWindowHandleSnapshot = $app.MainWindowHandle } catch { $mainWindowHandleSnapshot = "unavailable: $($_.Exception.Message)" }
    if ($app.HasExited) {
        $startupFailure = "Installed desktop application exited during startup (exit=$($app.ExitCode))"
    } elseif ($null -eq $window) {
        $startupFailure = 'Installed desktop application did not create a visible top-level window titled 小花鼠 within 30 seconds'
    } else {
        Write-Host "Installed desktop application created visible window handle $($window.Handle) (owner=$($window.Owner), class=$($window.ClassName))."
    }
} finally {
    if ($null -ne $app) {
        $app.Refresh()
        if (-not $app.HasExited) { Stop-Process -Id $app.Id -Force; $app.WaitForExit() }
    }
}
if ($startupFailure) {
    Write-Host "Startup process PID: $($app.Id); .NET MainWindowHandle snapshot: $mainWindowHandleSnapshot"
    if ($windows.Count -gt 0) {
        Write-Host 'Top-level windows owned by the application process:'
        $windows | Format-Table Handle, ProcessId, Visible, Owner, ClassName, Title -AutoSize | Out-String -Width 300 | Write-Host
    } else {
        Write-Host 'No top-level windows were enumerated for the application PID.'
    }
    Write-LogTail $appStdout 'Desktop startup stdout'
    Write-LogTail $appStderr 'Desktop startup stderr'
    throw $startupFailure
}
$uninstall = Start-Process -FilePath (Join-Path $install 'unins000.exe') -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') -PassThru
if (-not $uninstall.WaitForExit(120000)) { $uninstall.Kill(); throw 'Uninstaller timed out' }
if ($uninstall.ExitCode -ne 0) { throw "Uninstaller exited with $($uninstall.ExitCode)" }
for ($i = 0; $i -lt 20 -and (Test-Path $exe); $i++) { Start-Sleep -Seconds 1 }
if (Test-Path $exe) { throw 'Uninstaller did not remove the application executable' }
if (-not (Test-Path (Join-Path $data 'keep.txt'))) { throw 'Uninstaller removed application data' }
Write-Host 'Install, native startup, version, uninstall, and data preservation checks passed.'

$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true') { throw 'Run this installation smoke test only on an ephemeral GitHub runner.' }
$root = Split-Path -Parent $PSScriptRoot
$setup = Join-Path $root "dist\tamiops-$env:VERSION-windows-$env:ARCH-setup.exe"
$install = Join-Path $env:RUNNER_TEMP 'tamiops-installer-smoke'
$data = Join-Path $env:RUNNER_TEMP 'tamiops-installer-data'
if (Test-Path $install) { throw "Refusing to overwrite an existing test install: $install" }
New-Item -ItemType Directory -Path $data -Force | Out-Null
Set-Content -LiteralPath (Join-Path $data 'keep.txt') -Value 'Preserve application data'
$log = Join-Path $env:RUNNER_TEMP 'tamiops-installer.log'
$process = Start-Process -FilePath $setup -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', "/DIR=`"$install`"", "/LOG=`"$log`"") -PassThru
if (-not $process.WaitForExit(120000)) { $process.Kill(); throw 'Installer timed out' }
if ($process.ExitCode -ne 0) { Get-Content $log; throw "Installer exited with $($process.ExitCode)" }
$exe = Join-Path $install 'tamiops.exe'
foreach ($file in @('tamiops.exe', 'LICENSE', 'THIRD_PARTY_NOTICES.txt', 'unins000.exe')) {
    if (-not (Test-Path (Join-Path $install $file))) { throw "Installed payload missing $file" }
}
$stdout = Join-Path $env:RUNNER_TEMP 'tamiops-version.txt'
$versionProcess = Start-Process -FilePath $exe -ArgumentList '-version' -RedirectStandardOutput $stdout -PassThru
if (-not $versionProcess.WaitForExit(30000)) { $versionProcess.Kill(); throw 'Installed version probe timed out' }
if ($versionProcess.ExitCode -ne 0 -or (Get-Content $stdout -Raw).Trim() -ne $env:VERSION) { throw 'Installed application version mismatch' }
$app = Start-Process -FilePath $exe -ArgumentList @('-data-dir', "`"$data`"") -PassThru
try {
    Start-Sleep -Seconds 8
    $app.Refresh()
    if ($app.HasExited) { throw 'Installed desktop application exited during startup' }
    if ($app.MainWindowHandle -eq 0) { throw 'Installed desktop application did not create a native window' }
    Write-Host 'Installed desktop application created a native window.'
} finally {
    if (-not $app.HasExited) { Stop-Process -Id $app.Id -Force; $app.WaitForExit() }
}
$uninstall = Start-Process -FilePath (Join-Path $install 'unins000.exe') -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') -PassThru
if (-not $uninstall.WaitForExit(120000)) { $uninstall.Kill(); throw 'Uninstaller timed out' }
if ($uninstall.ExitCode -ne 0) { throw "Uninstaller exited with $($uninstall.ExitCode)" }
for ($i = 0; $i -lt 20 -and (Test-Path $exe); $i++) { Start-Sleep -Seconds 1 }
if (Test-Path $exe) { throw 'Uninstaller did not remove the application executable' }
if (-not (Test-Path (Join-Path $data 'keep.txt'))) { throw 'Uninstaller removed application data' }
Write-Host 'Install, native startup, version, uninstall, and data preservation checks passed.'

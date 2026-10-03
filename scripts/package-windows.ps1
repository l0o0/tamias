$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$version = $env:VERSION
$arch = $env:ARCH

if ([string]::IsNullOrWhiteSpace($version) -or $version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$') {
    throw 'VERSION must be a semantic version, for example 0.2.0-beta.1.'
}
if ($arch -ne 'amd64') {
    throw "ARCH must be amd64 for this package; received '$arch'."
}

$requiredFiles = @(
    (Join-Path $repoRoot 'bin\tamiops.exe'),
    (Join-Path $repoRoot 'LICENSE'),
    (Join-Path $repoRoot 'THIRD_PARTY_NOTICES.txt'),
    (Join-Path $repoRoot 'build\windows\tamiops.ico'),
    (Join-Path $repoRoot 'build\windows\tamiops.iss')
)
foreach ($path in $requiredFiles) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required Windows packaging input is missing: $path"
    }
}

$compiler = Get-Command 'ISCC.exe' -ErrorAction SilentlyContinue
if ($null -ne $compiler) {
    $iscc = $compiler.Source
} else {
    $programFilesX86 = [Environment]::GetEnvironmentVariable('ProgramFiles(x86)')
    $candidates = @(
        (Join-Path $programFilesX86 'Inno Setup 6\ISCC.exe'),
        (Join-Path $env:ProgramFiles 'Inno Setup 6\ISCC.exe')
    )
    $iscc = $candidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
}
if (-not $iscc) {
    throw 'Inno Setup 6 compiler ISCC.exe was not found. Install Inno Setup 6 or add ISCC.exe to PATH.'
}

$distDir = Join-Path $repoRoot 'dist'
$outputPath = Join-Path $distDir "tamiops-$version-windows-$arch-setup.exe"
New-Item -ItemType Directory -Path $distDir -Force | Out-Null
if (Test-Path -LiteralPath $outputPath) {
    Remove-Item -LiteralPath $outputPath -Force
}

Push-Location $repoRoot
try {
    & $iscc (Join-Path $repoRoot 'build\windows\tamiops.iss')
    if ($LASTEXITCODE -ne 0) {
        throw "Inno Setup compilation failed with exit code $LASTEXITCODE."
    }
} finally {
    Pop-Location
}

if (-not (Test-Path -LiteralPath $outputPath -PathType Leaf)) {
    throw "Inno Setup reported success but did not create the expected output: $outputPath"
}
Write-Host "Created $outputPath"

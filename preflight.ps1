$ErrorActionPreference = "Stop"

function Write-Status {
    param(
        [string]$Label,
        [string]$Status,
        [string]$Detail
    )

    $color = "Gray"
    if ($Status -eq "PASS") { $color = "Green" }
    elseif ($Status -eq "WARN") { $color = "Yellow" }
    elseif ($Status -eq "FAIL") { $color = "Red" }

    Write-Host ("[{0}] {1} - {2}" -f $Status, $Label, $Detail) -ForegroundColor $color
}

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$failCount = 0
$warnCount = 0

Write-Host "Running WhatsApp Agent preflight checks..." -ForegroundColor Cyan
Write-Host "Project root: $root" -ForegroundColor DarkCyan

# Go
$goCmd = Get-Command go -ErrorAction SilentlyContinue
if (-not $goCmd) {
    Write-Status "Go" "FAIL" "Go is not installed or not in PATH"
    $failCount++
} else {
    $goVerRaw = go version
    $goVer = [regex]::Match($goVerRaw, "go(\d+)\.(\d+)")
    if (-not $goVer.Success) {
        Write-Status "Go" "WARN" "Could not parse version from: $goVerRaw"
        $warnCount++
    } else {
        $major = [int]$goVer.Groups[1].Value
        $minor = [int]$goVer.Groups[2].Value
        if ($major -gt 1 -or ($major -eq 1 -and $minor -ge 22)) {
            Write-Status "Go" "PASS" "$goVerRaw"
        } else {
            Write-Status "Go" "FAIL" "Need Go >= 1.22, found: $goVerRaw"
            $failCount++
        }
    }
}

# Git
$gitCmd = Get-Command git -ErrorAction SilentlyContinue
if (-not $gitCmd) {
    Write-Status "Git" "FAIL" "Git is not installed or not in PATH"
    $failCount++
} else {
    Write-Status "Git" "PASS" "$(git --version)"
}

# GCC (needed for CGO sqlite3 path)
$gccCmd = Get-Command gcc -ErrorAction SilentlyContinue
if (-not $gccCmd) {
    Write-Status "GCC" "WARN" "gcc not found. Install MinGW-w64 if using sqlite3 (CGO)."
    $warnCount++
} else {
    Write-Status "GCC" "PASS" "$(gcc --version | Select-Object -First 1)"
}

# Env file
$envFile = Join-Path $root ".env"
if (Test-Path $envFile) {
    Write-Status ".env" "PASS" "$envFile found"
} else {
    Write-Status ".env" "WARN" "No .env found. Copy from .env.example before running app."
    $warnCount++
}

# Service account JSON path hint
$envExample = Join-Path $root ".env.example"
if (Test-Path $envExample) {
    Write-Status ".env.example" "PASS" "Template found"
} else {
    Write-Status ".env.example" "WARN" "Template not found"
    $warnCount++
}

Write-Host ""
if ($failCount -gt 0) {
    Write-Host "Preflight result: FAILED ($failCount fail, $warnCount warn)" -ForegroundColor Red
    exit 1
}

if ($warnCount -gt 0) {
    Write-Host "Preflight result: PASS with warnings ($warnCount warn)" -ForegroundColor Yellow
    exit 0
}

Write-Host "Preflight result: PASS" -ForegroundColor Green
exit 0

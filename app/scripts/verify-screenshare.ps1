# Autonomous self-check for screen share. ASCII-only (PowerShell 5.1 reads .ps1
# as ANSI; Cyrillic without BOM breaks the parser).
#
# Run:  powershell -ExecutionPolicy Bypass -File app\scripts\verify-screenshare.ps1
#
# Runs everything verifiable WITHOUT a GUI and WITHOUT a second machine:
#   1. build (compile)
#   2. unit tests: reassembly (single/multi/out-of-order/interleave+stale-guard/garbage)
#   3. unit tests: adaptive controller (convergence/bounds/recovery)
#   4. end-to-end P2P over loopback via real pion DataChannel (1 frame + 50-frame stream)
#   5. live benchmark of real screen capture+encode on this machine
#
# Exit code 0 = all green.

$ErrorActionPreference = "Stop"
$appDir = Split-Path -Parent $PSScriptRoot   # ...\app
Set-Location $appDir

$tags = "nx_dfn,nx_rnnoise,nx_onnx,voxdebug"
$fail = 0

function Step($name, $block) {
    Write-Host "`n=== $name ===" -ForegroundColor Cyan
    & $block
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED: $name (exit $LASTEXITCODE)" -ForegroundColor Red
        $script:fail = 1
    } else {
        Write-Host "OK: $name" -ForegroundColor Green
    }
}

Step "build" { go build ./... }

Step "unit: h264 encoder (openh264 static lib, I420 conversion)" {
    go test ./internal/codec/h264enc/ -count=1
}

Step "unit: reassembly + adaptive + control frames" {
    go test ./internal/services/ -run "TestReassemble|TestIngest|TestAdaptBitrate|TestScreenControl" -count=1
}

Step "e2e: P2P screen share over loopback" {
    go test -tags $tags ./internal/services/ -run "TestScreenShareP2P" -count=1 -timeout 120s -v
}

Step "live: real screen capture benchmark" {
    $env:VOX_SCREEN_BENCH = "1"
    go test ./internal/services/ -run "TestScreenCaptureBench" -count=1 -v
    Remove-Item Env:\VOX_SCREEN_BENCH -ErrorAction SilentlyContinue
}

if ($fail -eq 0) {
    Write-Host "`n==================== ALL GREEN ====================" -ForegroundColor Green
    exit 0
} else {
    Write-Host "`n==================== FAILURES ====================" -ForegroundColor Red
    exit 1
}

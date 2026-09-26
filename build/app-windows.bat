@echo off
REM Build Vox app for Windows.
REM   app-windows.bat                  → dev сборка (без version, без auto-update, без NS моделей)
REM   app-windows.bat v0.00.000.0040   → релизная сборка для отправки друзьям
REM
REM Релизная сборка кладёт в build\out\release\:
REM   vox.exe                              — с тегами nx_rnnoise nx_onnx (+ nx_dfn если есть libdf.a)
REM   onnxruntime.dll                      — рантайм для Silero VAD
REM   models\VAD\silero_vad.onnx           — модель VAD
REM   models\DeepFilterNet3.tar.gz         — модель DFN (используется если nx_dfn включён)
REM
REM Зипуй папку release\ и шли — дружище распакует и запустит vox.exe.
REM
REM Requires: Go, GCC/MinGW (для CGO/SQLCipher)
setlocal enabledelayedexpansion

set SCRIPT_DIR=%~dp0
set PROJECT_ROOT=%SCRIPT_DIR%..
set OUT_DIR=%SCRIPT_DIR%out
set VERSION=%~1

if "%VERSION%"=="" goto :dev_build

REM ---------------- RELEASE BUILD ----------------
set RELEASE_DIR=%OUT_DIR%\release
echo Building release %VERSION% for Windows...

REM Validate version starts with 'v' (минимальный sanity check)
echo %VERSION% | findstr /B /C:"v" >nul
if errorlevel 1 (
    echo error: version must start with 'v' ^(e.g. v0.00.000.0040^)
    exit /b 1
)

REM Read public key for auto-update (single base64 line)
set PUB_KEY_FILE=%PROJECT_ROOT%\build\release-sign.pub
if not exist "%PUB_KEY_FILE%" (
    echo error: missing %PUB_KEY_FILE%
    echo Без публичного ключа auto-update в клиенте не работает.
    exit /b 1
)
set /p PUBKEY=<"%PUB_KEY_FILE%"

REM Build tags: nx_rnnoise + nx_onnx всегда; nx_dfn — только если есть libdf.a
set TAGS=nx_rnnoise nx_onnx
set LIBDF_A=%PROJECT_ROOT%\third_party\libdf\target\x86_64-pc-windows-gnu\release\libdf.a
if exist "%LIBDF_A%" (
    set TAGS=nx_dfn !TAGS!
    echo libdf.a found — building with nx_dfn
) else (
    echo libdf.a not found — building without nx_dfn ^(NS fallback to RNNoise^)
)

if exist "%RELEASE_DIR%" rmdir /s /q "%RELEASE_DIR%"
mkdir "%RELEASE_DIR%"
mkdir "%RELEASE_DIR%\models\VAD"

cd /d "%PROJECT_ROOT%\app"
set CGO_ENABLED=1
go build -tags "!TAGS!" -ldflags "-H windowsgui -X main.AppVersion=%VERSION% -X main.UpdatePublicKey=%PUBKEY%" -o "%RELEASE_DIR%\vox.exe" .
if errorlevel 1 (
    echo BUILD FAILED
    exit /b 1
)

REM Bundle assets
copy /y "%PROJECT_ROOT%\app\internal\ml\onnx\lib\windows-amd64\onnxruntime.dll" "%RELEASE_DIR%\" >nul || echo warning: onnxruntime.dll missing
copy /y "%PROJECT_ROOT%\.local\models\VAD\silero_vad.onnx" "%RELEASE_DIR%\models\VAD\" >nul || echo warning: silero_vad.onnx missing
copy /y "%PROJECT_ROOT%\.local\models\DeepFilterNet3.tar.gz" "%RELEASE_DIR%\models\" >nul || echo warning: DeepFilterNet3.tar.gz missing

echo.
echo ===============================================
echo Release build complete: %RELEASE_DIR%
echo Contents:
dir /b "%RELEASE_DIR%"
echo ===============================================
echo Zip the folder and ship it.
goto :eof

REM ---------------- DEV BUILD ----------------
:dev_build
set DEV_DIR=%OUT_DIR%\dev
echo Building Vox app for Windows ^(dev^)...
if not exist "%DEV_DIR%" mkdir "%DEV_DIR%"
cd /d "%PROJECT_ROOT%\app"
set CGO_ENABLED=1
go build -ldflags="-H windowsgui" -o "%DEV_DIR%\vox.exe" .
if errorlevel 1 (
    echo BUILD FAILED
    exit /b 1
)
echo Done: %DEV_DIR%\vox.exe ^(dev — no auto-update, no NS models^)
echo For release build: app-windows.bat v0.00.000.XXXX

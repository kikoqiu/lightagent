@echo off
setlocal enableextensions

REM ===========================================================================
REM  build.bat - cross compile Go binaries from Windows (cmd.exe)
REM
REM  Location: <repo root>\build.bat
REM  Targets : windows/amd64, linux/amd64, linux/arm (GOARM=7), linux/arm64
REM  CGO     : disabled (CGO_ENABLED=0) -> static, self-contained binaries
REM  Output  : <repo root>\build\<app>-<goos>-<goarch>[<goarm>][.exe]
REM
REM  Usage   : build.bat
REM ===========================================================================

REM --- Config ------------------------------------------------------------------
REM Leave APP_NAME empty to use the project folder name.
set "APP_NAME="
REM e.g. set "VERSION=1.2.3" to inject it via -ldflags -X main.version=...
set "VERSION="
set "GOARM_DEFAULT=7"

set "ROOT_DIR=%~dp0"
pushd "%ROOT_DIR%" || (echo [ERROR] cannot open project root & exit /b 1)

if not defined APP_NAME for %%I in ("%CD%") do set "APP_NAME=%%~nxI"
set "OUT_DIR=%ROOT_DIR%build"
if not exist "%OUT_DIR%" mkdir "%OUT_DIR%"

REM --- Environment shared by every target --------------------------------------
set "CGO_ENABLED=0"
set "GOFLAGS=-trimpath"
set "LDFLAGS=-s -w"
if defined VERSION set "LDFLAGS=%LDFLAGS% -X main.version=%VERSION%"

where go >nul 2>nul || (echo [ERROR] "go" not found in PATH & popd & exit /b 1)

echo ============================================================
echo  app     : %APP_NAME%
echo  root    : %CD%
echo  output  : %OUT_DIR%
echo  version : %VERSION%
echo ============================================================

set "FAILED="

call :build windows amd64 "" .exe
call :build linux   amd64 "" ""
call :build linux   arm64 "" ""

echo.
if defined FAILED (
  echo [FAILED]%FAILED%
  popd
  exit /b 1
)
echo [OK] all targets built
echo.
dir /b "%OUT_DIR%"
popd
exit /b 0

REM --- :build <goos> <goarch> <goarm> <exe-suffix> -------------------------------
:build
setlocal
set "T_GOOS=%~1"
set "T_GOARCH=%~2"
set "T_GOARM=%~3"
set "T_EXE=%~4"
set "T_NAME=%APP_NAME%-%T_GOOS%-%T_GOARCH%%T_GOARM%%T_EXE%"
set "T_OUT=%OUT_DIR%\%T_NAME%"
set "GOOS=%T_GOOS%"
set "GOARCH=%T_GOARCH%"
if defined T_GOARM (set "GOARM=%T_GOARM%") else (set "GOARM=")

echo.
echo [build] %T_GOOS%/%T_GOARCH% GOARM=%T_GOARM% -^> %T_NAME%
go build -ldflags "%LDFLAGS%" -o "%T_OUT%" .
if errorlevel 1 (
  echo [ERROR] build failed: %T_NAME%
  endlocal & set "FAILED=%FAILED% %T_NAME%"
  exit /b 1
)
endlocal
exit /b 0

@echo off
rem Windows CMD launcher script for TORQ kvdb binary
set ROOT_DIR=%~dp0
set BIN=%ROOT_DIR%bin\kvdb.exe

if not exist "%ROOT_DIR%bin" mkdir "%ROOT_DIR%bin"
if not exist "%BIN%" (
    pushd "%ROOT_DIR%"
    go build -o bin\kvdb.exe .\cmd\kvdb
    popd
)

if exist "%BIN%" (
    "%BIN%" %*
) else (
    echo Error: bin\kvdb.exe not found. Build it with: go build -o bin\kvdb.exe .\cmd\kvdb >&2
    exit /b 1
)

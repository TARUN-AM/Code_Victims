@echo off
rem Windows CMD launcher script for TORQ torture test harness
set ROOT_DIR=%~dp0
python "%ROOT_DIR%torture\torture.py" %*

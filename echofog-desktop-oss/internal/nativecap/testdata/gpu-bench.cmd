@echo off
rem Build the bench next to this file: go test -c -o nativecap-bench.exe ./internal/nativecap
rem EchoFog GPU bench: captures this computer's screen and encodes it with its graphics card,
rem as a shared screen in a call, then writes what it measured to result.txt.
rem Play a video or a game on the main screen while it runs (about a minute), then send result.txt.
cd /d "%~dp0"
echo EchoFog GPU bench > result.txt
powershell -NoProfile -Command "Get-CimInstance Win32_VideoController | Format-Table Name, DriverVersion -AutoSize | Out-String; (Get-CimInstance Win32_Processor).Name; [Environment]::OSVersion.VersionString" >> result.txt
set ECHOFOG_STREAM_BENCH=monitor:0
set ECHOFOG_STREAM_BENCH_LEVELS=1
set ECHOFOG_STREAM_BENCH_SECONDS=12

echo. >> result.txt
echo === 1080p60, 12 Mbit/s, halved after 6 s >> result.txt
set ECHOFOG_STREAM_BENCH_SIZE=1920x1080
set ECHOFOG_STREAM_BENCH_FPS=60
set ECHOFOG_STREAM_BENCH_KBPS=12000
"%~dp0nativecap-bench.exe" -test.run=TestStreamBench -test.v >> result.txt 2>&1

echo. >> result.txt
echo === 1080p30, 7 Mbit/s, the rate moving every half second >> result.txt
set ECHOFOG_STREAM_BENCH_FPS=30
set ECHOFOG_STREAM_BENCH_KBPS=7000
set ECHOFOG_STREAM_BENCH_FLAP=1
"%~dp0nativecap-bench.exe" -test.run=TestStreamBench -test.v >> result.txt 2>&1
set ECHOFOG_STREAM_BENCH_FLAP=

echo. >> result.txt
echo === 720p60, 6 Mbit/s >> result.txt
set ECHOFOG_STREAM_BENCH_SIZE=1280x720
set ECHOFOG_STREAM_BENCH_FPS=60
set ECHOFOG_STREAM_BENCH_KBPS=6000
"%~dp0nativecap-bench.exe" -test.run=TestStreamBench -test.v >> result.txt 2>&1

echo. >> result.txt
echo === sources and the window list >> result.txt
"%~dp0nativecap-bench.exe" -test.run=TestSourcesListsMonitors -test.v >> result.txt 2>&1

echo Done: send result.txt
pause

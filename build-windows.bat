@echo off
rem 编译 Windows 服务端（需已安装 Go）
cd /d %~dp0
go build -trimpath -ldflags "-s -w" -o peacefulVA-server.exe .
if %errorlevel% neq 0 (
  echo 编译失败
  exit /b 1
)
echo 完成: peacefulVA-server.exe

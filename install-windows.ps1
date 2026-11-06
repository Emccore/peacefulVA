# peacefulVA Windows 安装/升级/修复/卸载脚本（需管理员 PowerShell 5+）
# 用法示例:
#   .\install-windows.ps1 -Install -Port 6767 -Tls manual          # 首次安装（内网自签名）
#   .\install-windows.ps1 -Install -Tls none -Port 6767 -TrustProxy -SecureCookies   # Cloudflare 回源
#   .\install-windows.ps1 -Install -Build                          # 无现成二进制时用 Go 现场编译
#   .\install-windows.ps1 -Upgrade                                 # 只换二进制，配置数据不动
#   .\install-windows.ps1 -Repair                                  # 环境修复（见下方说明）
#   .\install-windows.ps1 -Uninstall                               # 卸载服务
#   .\install-windows.ps1 -Uninstall -Purge                        # 卸载并删除全部数据
#
# -Repair 修复内容: 缺二进制补装(可现场编译)、缺配置补生成、缺证书重签、
#                   users.json 损坏自动回滚最近备份、防火墙规则重建、
#                   残留运行锁清理、服务重新注册并做健康检查
param(
  [switch]$Install,
  [switch]$Upgrade,
  [switch]$Repair,
  [switch]$Uninstall,
  [switch]$Purge,
  [switch]$Build,
  [int]$Port = 6767,
  [string]$Tls = "manual",
  [string]$Domain = "",
  [string]$RegistrationCode = "",
  [switch]$TrustProxy,
  [switch]$SecureCookies,
  [switch]$NoRegistration
)

$ErrorActionPreference = "Stop"
$SvcName = "peacefulVA"
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$InstallDir = Join-Path $env:ProgramFiles "peacefulVA"
$ExeDst = Join-Path $InstallDir "peacefulVA-server.exe"
$Cfg = Join-Path $InstallDir "config.json"
$DataDir = Join-Path $InstallDir "data"
$CertsDir = Join-Path $InstallDir "certs"

function Info($m) { Write-Host "[peacefulVA] $m" }
function Die($m) { Write-Host "[peacefulVA] 错误: $m" -ForegroundColor Red; exit 1 }

# ---- 管理员校验 ----
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Die "请用管理员身份运行 PowerShell"
}

# ---- 公共步骤：定位二进制 ----
function Get-ReadyBinary {
  $src = Join-Path $ScriptDir "peacefulVA-server.exe"
  if (-not (Test-Path $src) -or $Build) {
    if (-not (Test-Path (Join-Path $ScriptDir "go.mod"))) {
      Die "脚本目录下没有 go.mod，无法现场编译；请放置 peacefulVA-server.exe"
    }
    Info "现场编译（需已安装 Go）..."
    Push-Location $ScriptDir
    go build -trimpath -ldflags "-s -w" -o $ExeSrc .
    $code = $LASTEXITCODE
    Pop-Location
    if ($code -ne 0) { Die "编译失败，请安装 Go（golang.org/dl）或放置现成二进制" }
  }
  return $src
}

function Ensure-Dirs {
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
  New-Item -ItemType Directory -Force -Path $CertsDir | Out-Null
}

function Ensure-Binary {
  param([string]$Src)
  $need = -not (Test-Path $ExeDst)
  if (-not $need) {
    $s = (Get-Item $Src).Length; $d = (Get-Item $ExeDst).Length
    if ($s -ne $d) { $need = $true }  # 大小不同视为新版本
  }
  if ($need) {
    Stop-SvcWait
    Copy-Item $Src $ExeDst -Force
    Info "已部署二进制 -> $ExeDst"
  } else {
    Info "二进制已是最新，跳过"
  }
}

function Ensure-Config {
  if (Test-Path $Cfg) { Info "配置已存在，保持不变: $Cfg"; return }
  # JSON 里用正斜杠路径，规避反斜杠转义问题
  $dir = $InstallDir -replace '\\', '/'
  $regCode = if ($RegistrationCode) { $RegistrationCode } else { "" }
  $openReg = if ($NoRegistration) { "false" } else { "true" }
  $tp = if ($TrustProxy) { "true" } else { "false" }
  $sc = if ($SecureCookies) { "true" } else { "false" }
  $dom = if ($Domain) { $Domain } else { "" }
  $json = @"
{
  "addr": ":$Port",
  "tls_mode": "$Tls",
  "cert_file": "$dir/certs/cert.pem",
  "key_file": "$dir/certs/key.pem",
  "domain": "$dom",
  "cert_cache": "$dir/certs",
  "data_dir": "$dir/data",
  "open_registration": $openReg,
  "registration_code": "$regCode",
  "trust_proxy": $tp,
  "secure_cookies": $sc,
  "ice_servers": ["stun:stun.l.google.com:19302"],
  "turn_url": "",
  "turn_user": "",
  "turn_pass": "",
  "log_ips": false
}
"@
  # PS5 的 UTF8 带 BOM，Go 的 json 解析会失败，这里写无 BOM UTF8
  [System.IO.File]::WriteAllText($Cfg, $json, (New-Object System.Text.UTF8Encoding($false)))
  Info "已生成配置 $Cfg"
}

function Ensure-Cert {
  if ($Tls -ne "manual") { return }
  if (Test-Path (Join-Path $CertsDir "cert.pem")) { return }
  Info "生成自签名证书..."
  Push-Location $CertsDir
  & $ExeDst -gen-cert | Out-Null
  Pop-Location
  if ($LASTEXITCODE -ne 0) { Die "证书生成失败" }
}

function Ensure-Firewall {
  Remove-NetFirewallRule -DisplayName "peacefulVA" -ErrorAction SilentlyContinue
  New-NetFirewallRule -DisplayName "peacefulVA" -Direction Inbound -Protocol TCP -LocalPort $Port -Action Allow | Out-Null
  Info "防火墙已放行 TCP $Port"
}

function Register-Service {
  $bin = "`"$ExeDst`" -svc run -config `"$Cfg`""
  $exists = Get-Service -Name $SvcName -ErrorAction SilentlyContinue
  if ($exists) {
    Info "服务已存在，跳过注册"
    return
  }
  New-Service -Name $SvcName -BinaryPathName $bin -DisplayName "peacefulVA" `
    -Description "peacefulVA 加密音视频聊天服务" -StartupType Automatic | Out-Null
  # 崩溃自动重启：5s/5s/30s
  sc.exe failure $SvcName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null
  Info "服务已注册（开机自启，崩溃自动重启）"
}

function Stop-SvcWait {
  Stop-Service -Name $SvcName -Force -ErrorAction SilentlyContinue
  for ($i = 0; $i -lt 10; $i++) {
    if ((Get-Service -Name $SvcName -ErrorAction SilentlyContinue).Status -eq "Stopped") { break }
    Start-Sleep -Milliseconds 500
  }
}

function Invoke-HealthCheck {
  $scheme = if ($Tls -eq "none") { "http" } else { "https" }
  $ok = $false
  for ($i = 0; $i -lt 12; $i++) {
    $st = (Get-Service -Name $SvcName -ErrorAction SilentlyContinue).Status
    if ($st -ne "Running") {
      Start-Service -Name $SvcName -ErrorAction SilentlyContinue
      Start-Sleep -Seconds 1
      continue
    }
    $r = & curl.exe "-k" "-s" "-o" "NUL" "-w" "%{http_code}" "${scheme}://127.0.0.1:${Port}/healthz" 2>$null
    if ("$r" -eq "200") { $ok = $true; break }
    Start-Sleep -Seconds 1
  }
  if ($ok) { Info "健康检查通过: ${scheme}://127.0.0.1:${Port}/healthz -> 200" }
  else { Die "健康检查未通过，查看日志: Get-EventLog Application 或直接前台运行 $ExeDst -config $Cfg" }
}

# ---- 卸载 ----
if ($Uninstall) {
  Info "停止并删除服务..."
  Stop-SvcWait
  sc.exe delete $SvcName | Out-Null
  Remove-NetFirewallRule -DisplayName "peacefulVA" -ErrorAction SilentlyContinue
  if ($Purge) {
    if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
    Info "已卸载并清除全部数据"
  } else {
    Info "已卸载服务，账号数据保留在 $DataDir，彻底删除请加 -Purge"
  }
  exit 0
}

# ---- 修复模式 ----
if ($Repair) {
  Info "开始环境修复..."
  $svcExists = Get-Service -Name $SvcName -ErrorAction SilentlyContinue
  if ($svcExists) { Stop-SvcWait }
  # 残留运行锁
  $lock = Join-Path $DataDir ".lock"
  if (Test-Path $lock) { Remove-Item -Force $lock; Info "已清理残留运行锁" }
  Ensure-Dirs
  # 二进制
  if (-not (Test-Path $ExeDst)) {
    $src = Get-ReadyBinary
    Copy-Item $src $ExeDst -Force
    Info "已补装缺失的二进制"
  }
  # 配置
  if (-not (Test-Path $Cfg)) {
    Ensure-Config
    Info "已补生成缺失的配置"
  }
  # 证书
  if ($Tls -eq "manual" -and -not (Test-Path (Join-Path $CertsDir "cert.pem"))) {
    Push-Location $CertsDir
    & $ExeDst -gen-cert | Out-Null
    Pop-Location
    Info "已重签自签名证书"
  }
  # 账号数据完整性：损坏则回滚最近备份，无备份时隔离损坏文件保证服务可启动
  $usersJson = Join-Path $DataDir "users.json"
  if (Test-Path $usersJson) {
    $valid = $true
    try { Get-Content $usersJson -Raw | ConvertFrom-Json | Out-Null } catch { $valid = $false }
    if (-not $valid) {
      Info "users.json 已损坏，尝试回滚备份..."
      $bk = Get-ChildItem (Join-Path $DataDir "backups") -Directory -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending | Select-Object -First 1
      if ($bk) {
        foreach ($f in @("users.json", "sessions.json")) {
          $src = Join-Path $bk.FullName $f
          if (Test-Path $src) { Copy-Item $src (Join-Path $DataDir $f) -Force }
        }
        Info "已回滚到备份 $($bk.Name)"
      } else {
        Move-Item $usersJson "$usersJson.corrupt" -Force
        Info "没有可用备份，损坏文件已移为 users.json.corrupt（账号不可恢复，服务以空数据重启）"
      }
    }
  }
  Ensure-Firewall
  if (-not $svcExists) { Register-Service }
  Invoke-HealthCheck
  Info "修复完成"
  exit 0
}

# ---- 安装 / 升级 ----
if ($Install -or $Upgrade) {
  $src = Get-ReadyBinary
  Ensure-Dirs
  Ensure-Binary -Src $src
  Ensure-Config
  Ensure-Cert
  Ensure-Firewall
  Register-Service
  Invoke-HealthCheck
  Info "----------------------------------------"
  Info "安装完成"
  Info "  本机访问:   $(if ($Tls -eq 'none') { 'http' } else { 'https' })://127.0.0.1:$Port"
  Info "  服务管理:   Get-Service $SvcName / Restart-Service $SvcName"
  Info "  卸载:       .\install-windows.ps1 -Uninstall [-Purge]"
  exit 0
}

Die "未指定动作：-Install / -Upgrade / -Repair / -Uninstall"

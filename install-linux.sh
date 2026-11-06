#!/usr/bin/env bash
#
# peacefulVA Linux 一键安装/升级/修复/卸载脚本（独立文件）
# 环境自动搭建: --build 在无二进制时自动装 Go 并从源码编译
# 修复功能:     --repair 一键修复（详见下方说明）
#
# 用法:
#   sudo ./install-linux.sh                                    # 安装或升级，配置/数据不动
#   sudo ./install-linux.sh --build                            # 无二进制时自动装 Go 并编译
#   sudo ./install-linux.sh --port 6767 --tls manual           # 首次安装指定选项
#   sudo ./install-linux.sh --tls none --trust-proxy --secure-cookies   # Cloudflare 回源
#   sudo ./install-linux.sh --tls acme --domain chat.example.com --port 443
#   sudo ./install-linux.sh --registration-code XXXX           # 开启注册码
#   sudo ./install-linux.sh --no-registration                  # 关闭注册
#   sudo ./install-linux.sh --repair                           # 修复（见下）
#   sudo ./install-linux.sh --uninstall                        # 卸载服务（保留数据）
#   sudo ./install-linux.sh --uninstall --purge                # 卸载并删除数据
#
# --repair 修复内容: 缺二进制补装(可自动编译)、缺配置补生成、缺证书重签、
#   users.json 损坏自动回滚最近备份、残留运行锁清理、防火墙端口提示、
#   systemd 单元重写、服务重启并做健康检查
set -euo pipefail

INSTALL_DIR=/opt/peacefulva
RUN_USER=peacefulva
SERVICE=peacefulva
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

PORT=6767
TLS_MODE=manual
DOMAIN=""
CERT_HOSTS=""
TRUST_PROXY=false
SECURE_COOKIES=false
OPEN_REG=true
REG_CODE=""
DO_BUILD=false
REPAIR=false
UNINSTALL=false
PURGE=false

usage() { sed -n '3,22p' "$0"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --tls) TLS_MODE="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --hosts) CERT_HOSTS="$2"; shift 2 ;;
    --trust-proxy) TRUST_PROXY=true; shift ;;
    --secure-cookies) SECURE_COOKIES=true; shift ;;
    --registration-code) REG_CODE="$2"; OPEN_REG=true; shift 2 ;;
    --no-registration) OPEN_REG=false; shift ;;
    --build) DO_BUILD=true; shift ;;
    --repair) REPAIR=true; shift ;;
    --uninstall) UNINSTALL=true; shift ;;
    --purge) PURGE=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1 (用 --help 查看用法)"; exit 1 ;;
  esac
done

say() { echo -e "[peacefulVA] $*"; }
die() { echo -e "[peacefulVA] 错误: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 ]] || die "请用 root 运行 (sudo ./install-linux.sh)"

# ---------- 卸载 ----------
if $UNINSTALL; then
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  rm -f "/etc/systemd/system/${SERVICE}.service"
  systemctl daemon-reload
  rm -f "${INSTALL_DIR}/peacefulVA-server"
  if $PURGE; then
    rm -rf "$INSTALL_DIR"
    id "$RUN_USER" &>/dev/null && userdel "$RUN_USER" || true
    say "已卸载并清除全部数据"
  else
    say "已卸载服务，账号数据保留在 ${INSTALL_DIR}/data ，彻底删除请加 --purge"
  fi
  exit 0
fi

command -v systemctl &>/dev/null || die "需要 systemd"

# ---------- 环境自建：无二进制时自动装 Go 并编译 ----------
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) GOARCH_V=amd64 ;;
  aarch64|arm64) GOARCH_V=arm64 ;;
  *) die "不支持的架构: $ARCH" ;;
esac

build_from_source() {
  [[ -f "${SCRIPT_DIR}/go.mod" ]] || die "源码编译需要 go.mod，脚本须放在源码目录"
  if ! command -v go &>/dev/null; then
    say "未安装 Go，尝试自动安装..."
    if command -v apt-get &>/dev/null; then
      apt-get update -y && apt-get install -y golang-go || die "apt 安装 Go 失败，请手动安装"
    elif command -v dnf &>/dev/null; then
      dnf install -y golang || die "dnf 安装 Go 失败，请手动安装"
    elif command -v yum &>/dev/null; then
      yum install -y golang || die "yum 安装 Go 失败，请手动安装"
    else
      die "无法自动安装 Go，请手动安装后重试（版本 >= 1.22）"
    fi
  fi
  say "从源码编译（GOOS=linux GOARCH=${GOARCH_V}）..."
  (cd "${SCRIPT_DIR}" && GOOS=linux GOARCH="${GOARCH_V}" \
    GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}" \
    go build -trimpath -ldflags "-s -w" -o "${INSTALL_DIR}/peacefulVA-server.new" .) \
    || die "编译失败"
  mv "${INSTALL_DIR}/peacefulVA-server.new" "${INSTALL_DIR}/peacefulVA-server"
}

ensure_binary() {
  local src="${SCRIPT_DIR}/peacefulVA-server-linux-${GOARCH_V}"
  if $DO_BUILD; then
    build_from_source
    return
  fi
  if [[ -f "$src" ]]; then
    install -m 0755 "$src" "${INSTALL_DIR}/peacefulVA-server.new"
    mv "${INSTALL_DIR}/peacefulVA-server.new" "${INSTALL_DIR}/peacefulVA-server"
    return 0
  fi
  if [[ ! -f "${INSTALL_DIR}/peacefulVA-server" ]]; then
    say "未找到二进制，自动进入源码编译模式（等价 --build）"
    DO_BUILD=true
    build_from_source
    return
  fi
  say "使用已安装的二进制"
}

gen_config() {
  local config="$1"
  [[ -f "$config" ]] && { say "配置已存在，保持不变: $config"; return 0; }
  [[ "$DOMAIN" =~ ^[A-Za-z0-9.-]*$ ]] || die "域名含非法字符"
  [[ "$REG_CODE" =~ "^[A-Za-z0-9@#%^&_=+-]*$" ]] || die "注册码含非法字符"
  cat > "$config" <<EOF
{
  "addr": ":${PORT}",
  "tls_mode": "${TLS_MODE}",
  "cert_file": "${INSTALL_DIR}/certs/cert.pem",
  "key_file": "${INSTALL_DIR}/certs/key.pem",
  "domain": "${DOMAIN}",
  "cert_cache": "${INSTALL_DIR}/certs",
  "data_dir": "${INSTALL_DIR}/data",
  "open_registration": ${OPEN_REG},
  "registration_code": "${REG_CODE}",
  "trust_proxy": ${TRUST_PROXY},
  "secure_cookies": ${SECURE_COOKIES},
  "ice_servers": ["stun:stun.l.google.com:19302"],
  "turn_url": "",
  "turn_user": "",
  "turn_pass": "",
  "log_ips": false
}
EOF
  chown "${RUN_USER}:${RUN_USER}" "$config"
  chmod 600 "$config"
  say "已生成配置 $config"
}

ensure_cert() {
  [[ "$TLS_MODE" == "manual" ]] || return 0
  [[ -f "${INSTALL_DIR}/certs/cert.pem" ]] && return 0
  say "生成自签名证书..."
  (cd "${INSTALL_DIR}" && ./peacefulVA-server -gen-cert -hosts "${CERT_HOSTS}") || die "证书生成失败"
  mv "${INSTALL_DIR}/cert.pem" "${INSTALL_DIR}/key.pem" "${INSTALL_DIR}/certs/" 2>/dev/null || true
  [[ "$TLS_MODE" == "acme" && "$PORT" != "443" ]] && say "注意: acme 模式 Let's Encrypt 直连 443 验证，端口不是 443 时需确认有转发"
}

write_unit() {
  cat > "/etc/systemd/system/${SERVICE}.service" <<EOF
[Unit]
Description=peacefulVA Server (WebRTC + HTTPS)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/peacefulVA-server -config ${INSTALL_DIR}/config.json
WorkingDirectory=${INSTALL_DIR}
User=${RUN_USER}
Group=${RUN_USER}
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
ReadWritePaths=${INSTALL_DIR}/data ${INSTALL_DIR}/certs

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

health_check() {
  systemctl restart "$SERVICE"
  sleep 1
  local scheme="https"
  [[ "$TLS_MODE" == "none" ]] && scheme="http"
  local ok=false
  for i in 1 2 3 4 5; do
    if curl -k -s -f "${scheme}://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then ok=true; break; fi
    sleep 1
  done
  if $ok; then
    say "健康检查通过: ${scheme}://127.0.0.1:${PORT}/healthz"
  else
    say "健康检查未通过，最近日志:"; journalctl -u "$SERVICE" -n 15 --no-pager || true
    die "请按日志排查后重跑 --repair"
  fi
}

# ---------- 修复模式 ----------
if $REPAIR; then
  say "开始环境修复..."
  systemctl stop "$SERVICE" 2>/dev/null || true
  rm -f "${INSTALL_DIR}/data/.lock" || true
  mkdir -p "${INSTALL_DIR}/data" "${INSTALL_DIR}/certs"

  say "[1/6] 检查二进制..."
  ensure_binary

  say "[2/6] 检查配置..."
  gen_config "${INSTALL_DIR}/config.json"

  say "[3/6] 检查证书..."
  ensure_cert

  say "[4/6] 检查账号数据完整性..."
  if [[ -f "${INSTALL_DIR}/data/users.json" ]] && ! "${INSTALL_DIR}/peacefulVA-server" -config "${INSTALL_DIR}/config.json" -admin stats >/dev/null 2>&1; then
    say "  users.json 损坏，尝试回滚备份..."
    bk="$(ls -1d "${INSTALL_DIR}/data/backups"/* 2>/dev/null | sort | tail -1 || true)"
    if [[ -n "$bk" ]]; then
      cp -f "${bk}/users.json"    "${INSTALL_DIR}/data/users.json"    2>/dev/null || true
      cp -f "${bk}/sessions.json" "${INSTALL_DIR}/data/sessions.json" 2>/dev/null || true
      if "${INSTALL_DIR}/peacefulVA-server" -config "${INSTALL_DIR}/config.json" -admin stats >/dev/null 2>&1; then
        say "  已回滚到备份 $(basename "$bk")"
      else
        mv -f "${INSTALL_DIR}/data/users.json" "${INSTALL_DIR}/data/users.json.corrupt"
        say "  备份也损坏，已隔离为 users.json.corrupt（账号不可恢复，服务以空数据重启）"
      fi
    else
      mv -f "${INSTALL_DIR}/data/users.json" "${INSTALL_DIR}/data/users.json.corrupt"
      say "  没有可用备份，已隔离为 users.json.corrupt（账号不可恢复，服务以空数据重启）"
    fi
  else
    say "  账号数据正常"
  fi

  say "[5/6] 重写 systemd 单元..."
  write_unit

  say "[6/6] 重启并健康检查..."
  health_check

  say "修复完成"
  exit 0
fi

# ---------- 安装 / 升级 ----------
if ! id "$RUN_USER" &>/dev/null; then
  useradd -r -d "$INSTALL_DIR" -s /usr/sbin/nologin "$RUN_USER"
  say "已创建系统用户 ${RUN_USER}"
fi

mkdir -p "${INSTALL_DIR}/data" "${INSTALL_DIR}/certs"
systemctl stop "$SERVICE" 2>/dev/null || true

say "部署二进制..."
ensure_binary
chown -R "${RUN_USER}:${RUN_USER}" "$INSTALL_DIR"

gen_config "${INSTALL_DIR}/config.json"
ensure_cert
write_unit
systemctl enable --now "$SERVICE"
sleep 1
systemctl --no-pager --lines 5 status "$SERVICE" || true

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
scheme="https"; [[ "$TLS_MODE" == "none" ]] && scheme="http"
say "----------------------------------------"
say "安装完成"
say "  本机访问:   ${scheme}://127.0.0.1:${PORT}"
[[ -n "$IP" ]] && say "  局域网访问: ${scheme}://${IP}:${PORT}"
say "  防火墙放行: ufw allow ${PORT}/tcp   或   firewall-cmd --add-port=${PORT}/tcp --permanent"
say "  查看日志:   journalctl -u ${SERVICE} -f"
say "  环境异常:   sudo ./install-linux.sh --repair"

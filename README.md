# peacefulVA

基于 WebRTC 的浏览器端音视频通话服务。音视频在浏览器之间 P2P 直连（DTLS-SRTP 加密），服务器仅承担信令中转与页面托管；文字消息经 ECDH 密钥协商后端到端加密，服务器全程只接触密文。单二进制部署，无外部运行时依赖。

## 特性

- 账号体系：注册即分配全局唯一 ID，bcrypt 口令哈希，防用户名枚举
- 好友与在线状态：按 ID 互加好友，实时在线指示，仅好友之间可通话与通信
- 音视频通话：WebRTC 完美协商，视频/语音/屏幕共享，来电接听与拒绝
- 端到端加密聊天：ECDH P-256 + HKDF-SHA256 + AES-256-GCM，密钥不出浏览器，无消息留存
- 画质动态调节：通话中实时切换分辨率与编码码率上限，无需重新协商
- 零媒体中转：服务器带宽占用与在线人数、通话码率无关
- 低资源占用：静态链接单二进制（约 8 MB），ARM 设备可运行
- 反滥用设计：登录限流、每 IP 并发连接上限、消息速率限制、WebSocket 同源校验、CSP 全拒绝策略

## 架构

```
客户端 A <---- DTLS-SRTP (UDP, P2P 直连) ----> 客户端 B     媒体流，服务器不经手
    |                                              |
    +---------- WSS 信令与密文消息 (TCP) ----------+
                       |
                 Go 服务端 (:6767)
          账号存储 / 好友关系 / 呼叫中继 / 网页托管
```

- 信令通道：WebSocket over TLS，仅转发加密载荷（SDP/ICE 原样透传，聊天只传密文）
- 数据存储：本地 JSON 文件（原子写入），启动时自动滚动备份，保留最近 7 份
- NAT 兜底：可选 coturn 中继（UDP 13200–14200），与信令端口不冲突

## 快速开始

### 构建

需要 Go 1.22+：

```bash
go build -trimpath -ldflags "-s -w" -o peacefulVA-server-linux-amd64 .   # Linux
go build -trimpath -ldflags "-s -w" -o peacefulVA-server.exe .           # Windows
```

或使用脚本：`./build-linux.sh`（同时产出 amd64 与 arm64）、`build-windows.bat`。

### Linux 部署

将二进制与 `install-linux.sh` 放在同一目录，以 root 执行：

```bash
sudo ./install-linux.sh                          # 默认安装（:6767，自签名证书）
sudo ./install-linux.sh --build                  # 无二进制时自动安装 Go 并现场编译
sudo ./install-linux.sh --tls none --trust-proxy --secure-cookies   # Cloudflare 回源
sudo ./install-linux.sh --tls acme --domain chat.example.com --port 443
sudo ./install-linux.sh --registration-code XXXX # 启用注册码
sudo ./install-linux.sh --repair                 # 环境修复（见下文）
sudo ./install-linux.sh --uninstall [--purge]    # 卸载
```

安装内容：专用系统用户（nologin）、`/opt/peacefulva` 目录、systemd 单元（含沙箱加固与 `Restart=on-failure`）、自签名证书。重复执行即升级，配置与数据不受影响。

`--repair` 修复范围：补装二进制、补生成配置、重签证书、`users.json` 损坏时自动回滚最近备份（无备份则隔离损坏文件）、清理残留运行锁、重写 systemd 单元、重启并执行健康检查。

### Windows 部署

管理员 PowerShell 5+：

```powershell
.\install-windows.ps1 -Install -Port 6767 -Tls manual
.\install-windows.ps1 -Install -Tls none -Port 6767 -TrustProxy -SecureCookies
.\install-windows.ps1 -Build        # 无二进制时现场编译
.\install-windows.ps1 -Repair
.\install-windows.ps1 -Uninstall [-Purge]
```

安装内容：二进制部署至 `%ProgramFiles%\peacefulVA`、防火墙入站规则、Windows 服务注册（开机自启、崩溃 5 秒自动重启）、自签名证书。

### 运行时参数

```bash
peacefulVA-server -config config.json             # 前台运行
peacefulVA-server -port 6767                      # 覆盖配置中的监听端口
peacefulVA-server -gen-cert -hosts 192.168.1.10   # 生成自签名证书（ECDSA P-256，10 年）
peacefulVA-server -admin list                     # 管理命令，见下文
```

## 配置

```json
{
  "addr": ":6767",
  "tls_mode": "manual",
  "cert_file": "cert.pem",
  "key_file": "key.pem",
  "domain": "",
  "data_dir": "data",
  "open_registration": true,
  "registration_code": "",
  "trust_proxy": false,
  "secure_cookies": false,
  "max_conns_per_ip": 10,
  "ice_servers": ["stun:stun.l.google.com:19302"],
  "turn_url": "",
  "log_ips": false
}
```

| 字段 | 说明 |
|---|---|
| `tls_mode` | `manual`（自备/自签证书）、`acme`（Let's Encrypt 自动签发，TLS-ALPN 走同一端口）、`none`（明文，仅限本机调试或 Cloudflare Flexible 回源） |
| `open_registration` / `registration_code` | 注册开关与注册码；非空时注册必须携带（常量时间比较） |
| `trust_proxy` | 部署于 Cloudflare 等反向代理后时开启，从 `CF-Connecting-IP` 还原真实客户端 IP 用于限流 |
| `secure_cookies` | 反代 HTTPS 回源 HTTP（Flexible）时强制会话 Cookie 携带 Secure 标记 |
| `max_conns_per_ip` | 单 IP 并发 WebSocket 上限，防连接洪泛；同账号顶号重连豁免 |
| `ice_servers` / `turn_url` | 下发至浏览器的 STUN/TURN 配置 |
| `log_ips` | 默认不在日志中记录客户端 IP |

## 管理命令

```bash
peacefulVA-server -config config.json -admin list          # 账号列表
peacefulVA-server -admin stats                             # 账号 / 好友对 / 会话统计
peacefulVA-server -admin backup                            # 手动备份至 data/backups/
peacefulVA-server -admin passwd <用户名|VID> <新密码>       # 重置密码
peacefulVA-server -admin rename <用户名|VID> <新昵称>
peacefulVA-server -admin delete <用户名|VID>                # 删除账号（级联清理好友关系与会话）
peacefulVA-server -admin setregcode <注册码|->              # 设置/清除注册码（写入 config.json）
peacefulVA-server -admin setreg <open|close>                # 开/关注册
```

服务运行期间持有 `data/.lock`，写类命令将被拒绝（判活基于端口探测与 PID 双重校验，残留锁自动清除）；确认无碍可加 `-force`。

## Cloudflare 接入

1. DNS 添加 A/AAAA 记录并开启代理
2. 源站 `tls_mode: "none"`、`trust_proxy: true`、`secure_cookies: true`
3. Cloudflare Origin Rule 将目标端口指向 6767
4. SSL 模式选 Flexible；如需 Full (strict)，为源站配置 Cloudflare Origin CA 证书并切换 `manual` 模式

WebRTC 媒体为 UDP 直连，不经过 Cloudflare；STUN/TURN 由浏览器按 `ice_servers` 直接访问。

## 严格 NAT 环境

对称 NAT 等无法打洞的场景部署 coturn 中继（参考 `deploy/turnserver.conf.example`）：UDP 13200–14200，`turn_url`/凭证与 `config.json` 保持一致，防火墙放行对应 UDP 段。

## 音视频规格

| 视频 | 分辨率/帧率 | 码率上限 |
|---|---|---|
| 2160P·60 | 3840×2160 / 60 | 75 Mbps |
| 2160P·30 | 3840×2160 / 30 | 45 Mbps |
| 1080P·60 | 1920×1080 / 60 | 15 Mbps |
| 720P·60 | 1280×720 / 60 | 6 Mbps |
| 480P·30 | 854×480 / 30 | 2.5 Mbps |
| 360P·30 | 640×360 / 30 | 1.5 Mbps |

| 音频（Opus，48 kHz fullband） | 码率上限 |
|---|---|
| 极高 | 510 kbps |
| 较高 | 192 kbps |
| 普通 | 96 kbps |
| 电话音质 | 16 kbps |

通话中实时切换，经 `RTCRtpSender.setParameters` 与轨道 `applyConstraints` 实现，不触发重新协商；编码器为 H.264/VP9（浏览器协商），AV1 环境下实际带宽约为上述一半。

## 客户端使用

1. 访问服务地址，注册账号（自动分配 8 位唯一 ID）并登录
2. 复制自己的 ID 发送给对方，双方互加好友
3. 在好友列表中选择在线联系人：文字聊天（端到端加密）、视频通话、语音通话
4. 通话中可切换画质/音质、开关麦克风摄像头、共享屏幕；好友 ID 位于地址 `#` 之后，不进入服务器日志

聊天记录仅保存在浏览器内存中，刷新即清空；服务器不存储任何消息内容。

## 安全设计

- 传输：信令 TLS 1.3（HSTS、CSP 全拒绝、frame-ancestors none）；媒体 DTLS-SRTP
- 聊天加密：会话级 ECDH P-256 密钥对，HKDF-SHA256 派生（上下文绑定双方 VID 与房间无关），AES-256-GCM；服务器仅见密文
- 认证：bcrypt（cost 10），登录失败统一应答并垫假哈希防时序枚举；会话 Cookie HttpOnly + SameSite=Strict，256 位随机令牌
- 反滥用：登录 1 次/秒限流、连接限流、每 IP 并发上限、单连接 20 msg/s、32 KB 消息上限、WebSocket 同源校验、好友关系服务端强制校验
- 运行时：非 root + systemd 沙箱（Windows 服务亦可崩溃自恢复）；管理命令运行锁防双写

## 目录结构

```
main.go / service.go    服务入口，Windows 服务模式
hub.go                  WebSocket 信令中心
store.go                账号存储、备份、运行锁
api.go                  REST 接口
config.go               配置
gencert.go              自签名证书生成
static/                 内嵌客户端（无第三方请求）
install-linux.sh        Linux 安装/升级/修复/卸载
install-windows.ps1     Windows 安装/升级/修复/卸载
deploy/                 systemd 单元、coturn 参考配置
```

## 许可证

尚未设定开源许可证，保留所有权利。如需以特定许可证（MIT/Apache-2.0 等）发布，请补充 LICENSE 文件。

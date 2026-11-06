# peacefulVA

[English](README.en.md) | [中文](README.md)

Browser-based voice and video calling built on WebRTC. Media streams are peer-to-peer between browsers (DTLS-SRTP encrypted); the server only relays signaling and serves the web client. Text messages are end-to-end encrypted via ECDH key agreement — the server never sees plaintext. Single-binary deployment with no external runtime dependencies.

## Features

- Account system: globally unique ID assigned at registration, bcrypt password hashing, resistant to username enumeration
- Contacts and presence: add contacts by ID, real-time online status; calls and messages restricted to contacts, enforced server-side
- Calls: WebRTC perfect negotiation, video/voice calls, screen sharing, incoming call accept/decline
- End-to-end encrypted chat: ECDH P-256 + HKDF-SHA256 + AES-256-GCM; keys never leave the browser, no message retention
- In-call quality control: switch resolution and encoder bitrate caps on the fly, no renegotiation required
- Zero media relay: server bandwidth is independent of participant count and call bitrate
- Low footprint: statically linked single binary (~8 MB), runs on ARM devices
- Abuse resistance: login rate limiting, per-IP concurrent connection cap, per-connection message rate limit, same-origin WebSocket enforcement, fully restrictive CSP

## Architecture

```
Client A <---- DTLS-SRTP (UDP, direct P2P) ----> Client B     media, server never touches it
    |                                              |
    +---------- WSS signaling + ciphertext (TCP) --+
                       |
                 Go server (:6767)
      accounts / contacts / call signaling / web hosting
```

- Signaling: WebSocket over TLS; relays opaque payloads only (SDP/ICE forwarded as-is, chat as ciphertext)
- Storage: local JSON files with atomic writes; automatic rolling backup on startup (last 7 kept)
- NAT fallback: optional coturn relay (UDP 13200–14200), no conflict with the signaling port

## Quick Start

### Build

Requires Go 1.22+:

```bash
go build -trimpath -ldflags "-s -w" -o peacefulVA-server-linux-amd64 .   # Linux
go build -trimpath -ldflags "-s -w" -o peacefulVA-server.exe .           # Windows
```

Or use the scripts: `./build-linux.sh` (produces both amd64 and arm64), `build-windows.bat`.

### Linux Deployment

Place the binary and `install-linux.sh` in the same directory and run as root:

```bash
sudo ./install-linux.sh                          # default install (:6767, self-signed cert)
sudo ./install-linux.sh --build                  # auto-installs Go and builds if no binary
sudo ./install-linux.sh --tls none --trust-proxy --secure-cookies   # behind Cloudflare
sudo ./install-linux.sh --tls acme --domain chat.example.com --port 443
sudo ./install-linux.sh --registration-code XXXX # enable registration code
sudo ./install-linux.sh --repair                 # environment repair (see below)
sudo ./install-linux.sh --uninstall [--purge]    # uninstall
```

What it does: dedicated system user (nologin), `/opt/peacefulva` directory, hardened systemd unit (`Restart=on-failure`), self-signed certificate. Re-running performs an upgrade — configuration and data are preserved.

`--repair` covers: reinstall missing binary, regenerate missing config, re-sign certificates, restore `users.json` from the latest backup when corrupted (or quarantine it when no backup exists), clear stale runtime locks, rewrite the systemd unit, restart and run a health check.

### Windows Deployment

Admin PowerShell 5+:

```powershell
.\install-windows.ps1 -Install -Port 6767 -Tls manual
.\install-windows.ps1 -Install -Tls none -Port 6767 -TrustProxy -SecureCookies
.\install-windows.ps1 -Build        # build from source if no binary present
.\install-windows.ps1 -Repair
.\install-windows.ps1 -Uninstall [-Purge]
```

What it does: deploys the binary to `%ProgramFiles%\peacefulVA`, adds an inbound firewall rule, registers a Windows service (auto-start, auto-restart 5 s after crash), generates a self-signed certificate.

### Runtime Options

```bash
peacefulVA-server -config config.json             # run in foreground
peacefulVA-server -port 6767                      # override the listening port
peacefulVA-server -gen-cert -hosts 192.168.1.10   # self-signed certificate (ECDSA P-256, 10 years)
peacefulVA-server -admin list                     # admin commands, see below
```

## Configuration

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

| Field | Description |
|---|---|
| `tls_mode` | `manual` (bring your own / self-signed), `acme` (Let's Encrypt, TLS-ALPN on the same port), `none` (plaintext; local testing or Cloudflare Flexible origin only) |
| `open_registration` / `registration_code` | Registration switch and access code; when set, registration requires it (constant-time comparison) |
| `trust_proxy` | Enable behind Cloudflare or other reverse proxies to recover the real client IP (`CF-Connecting-IP`) for rate limiting |
| `secure_cookies` | Force the Secure flag on session cookies when HTTPS terminates at the proxy (Flexible) |
| `max_conns_per_ip` | Per-IP concurrent WebSocket cap against connection floods; same-account reconnects are exempt |
| `ice_servers` / `turn_url` | STUN/TURN configuration delivered to browsers |
| `log_ips` | Client IPs are not logged by default |

## Admin Commands

```bash
peacefulVA-server -config config.json -admin list          # list accounts
peacefulVA-server -admin stats                             # accounts / contact pairs / sessions
peacefulVA-server -admin backup                            # backup to data/backups/
peacefulVA-server -admin passwd <username|VID> <new>       # reset password
peacefulVA-server -admin rename <username|VID> <name>
peacefulVA-server -admin delete <username|VID>             # delete account (cascades contacts and sessions)
peacefulVA-server -admin setregcode <code|->               # set/clear registration code (writes config.json)
peacefulVA-server -admin setreg <open|close>               # toggle registration
```

While the server runs it holds `data/.lock`; write commands are refused (liveness is verified by port probe plus PID check; stale locks are cleared automatically). Use `-force` to override deliberately.

## Behind Cloudflare

1. Add a proxied A/AAAA record
2. Origin: `tls_mode: "none"`, `trust_proxy: true`, `secure_cookies: true`
3. Add a Cloudflare Origin Rule mapping the destination port to 6767
4. Set SSL mode to Flexible; for Full (strict), install a Cloudflare Origin CA certificate on the origin and switch to `manual`

WebRTC media is UDP and bypasses Cloudflare entirely; browsers reach STUN/TURN directly per `ice_servers`.

## Strict NAT Environments

For symmetric NAT scenarios where hole punching fails, deploy coturn (see `deploy/turnserver.conf.example`): UDP 13200–14200, credentials matching `config.json`, firewall open on the relay range.

## Media Specifications

| Video | Resolution / FPS | Bitrate cap |
|---|---|---|
| 2160P·60 | 3840×2160 / 60 | 75 Mbps |
| 2160P·30 | 3840×2160 / 30 | 45 Mbps |
| 1080P·60 | 1920×1080 / 60 | 15 Mbps |
| 720P·60 | 1280×720 / 60 | 6 Mbps |
| 480P·30 | 854×480 / 30 | 2.5 Mbps |
| 360P·30 | 640×360 / 30 | 1.5 Mbps |

| Audio (Opus, 48 kHz fullband) | Bitrate cap |
|---|---|
| Ultra | 510 kbps |
| High | 192 kbps |
| Standard | 96 kbps |
| Phone quality | 16 kbps |

Quality is switchable mid-call via `RTCRtpSender.setParameters` and track `applyConstraints` without renegotiation. The encoder is negotiated by the browser (H.264/VP9); on AV1-capable peers actual bandwidth is roughly half the listed values.

## Client Usage

1. Open the service URL, register (an 8-character unique ID is assigned) and sign in
2. Share your ID with the other party and add each other as contacts
3. Select an online contact: encrypted text chat, video call, or voice call
4. During a call: switch quality, toggle microphone/camera, share the screen. The room-less design keeps IDs out of server logs

Chat history lives only in browser memory and is cleared on refresh; no message content is stored server-side.

## Security Design

- Transport: signaling over TLS 1.3 (HSTS, fully restrictive CSP, `frame-ancestors 'none'`); media over DTLS-SRTP
- Chat encryption: session-scoped ECDH P-256 keypairs, HKDF-SHA256 derivation (context-bound to both VIDs), AES-256-GCM; the server relays ciphertext only
- Authentication: bcrypt (cost 10), uniform failure responses with a dummy-hash delay against timing enumeration; session cookies are HttpOnly + SameSite=Strict with 256-bit random tokens
- Abuse resistance: login rate of 1 req/s, connect rate limiting, per-IP concurrent cap, 20 msg/s per connection, 32 KB message cap, same-origin WebSocket enforcement, server-side contact validation on every relay
- Runtime: non-root execution with systemd sandboxing (Windows service with crash recovery); admin write commands guarded by a runtime lock

## Repository Layout

```
main.go / service.go    entry point, Windows service mode
hub.go                  WebSocket signaling hub
store.go                account storage, backups, runtime lock
api.go                  REST endpoints
config.go               configuration
gencert.go              self-signed certificate generation
static/                 embedded client (zero third-party requests)
install-linux.sh        Linux install / upgrade / repair / uninstall
install-windows.ps1     Windows install / upgrade / repair / uninstall
deploy/                 systemd unit, coturn reference configuration
```

## License

All rights reserved; no open-source license has been designated yet. Add a LICENSE file (MIT/Apache-2.0, etc.) before redistributing.

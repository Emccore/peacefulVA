package main

import (
	"encoding/json"
	"os"
)

// Config 服务端配置，字段含义见 config.json
type Config struct {
	Addr             string   `json:"addr"`               // 监听地址，默认 ":6767"
	TLSMode          string   `json:"tls_mode"`           // manual | acme | none(明文，仅本机调试或 Cloudflare Flexible 回源)
	CertFile         string   `json:"cert_file"`          // manual 模式证书
	KeyFile          string   `json:"key_file"`           // manual 模式私钥
	Domain           string   `json:"domain"`             // acme 模式域名（TLS-ALPN 验证走本端口）
	CertCache        string   `json:"cert_cache"`         // acme 证书缓存目录
	DataDir          string   `json:"data_dir"`           // 账号数据目录
	OpenRegistration bool     `json:"open_registration"`  // 是否开放注册
	RegistrationCode string   `json:"registration_code"`  // 注册码，非空则注册必须填写
	TrustProxy       bool     `json:"trust_proxy"`        // 挂 Cloudflare 等反代时开启，用真实客户端 IP 做限流
	SecureCookies    bool     `json:"secure_cookies"`     // 反代 HTTPS 回源 HTTP 时强制 Cookie Secure
	MaxConnsPerIP    int      `json:"max_conns_per_ip"`   // 单 IP 并发 WebSocket 上限，防连接洪泛
	IceServers       []string `json:"ice_servers"`        // STUN 列表，下发给浏览器
	TurnURL          string   `json:"turn_url"`           // 可选 TURN，严格 NAT 兜底
	TurnUser         string   `json:"turn_user"`
	TurnPass         string   `json:"turn_pass"`
	LogIPs           bool     `json:"log_ips"` // 是否在日志中记录客户端 IP（默认否）
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if cfg.Addr == "" {
		cfg.Addr = ":6767"
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = "manual"
	}
	if cfg.CertCache == "" {
		cfg.CertCache = "certs"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if len(cfg.IceServers) == 0 {
		cfg.IceServers = []string{"stun:stun.l.google.com:19302"}
	}
	if cfg.MaxConnsPerIP <= 0 {
		cfg.MaxConnsPerIP = 10
	}
	return cfg, nil
}

// clientConfig 下发给浏览器的 ICE 配置
func (c *Config) clientConfig() map[string]any {
	ice := make([]map[string]any, 0, len(c.IceServers)+1)
	for _, u := range c.IceServers {
		ice = append(ice, map[string]any{"urls": u})
	}
	if c.TurnURL != "" {
		ice = append(ice, map[string]any{"urls": c.TurnURL, "username": c.TurnUser, "credential": c.TurnPass})
	}
	return map[string]any{
		"iceServers":       ice,
		"regCodeRequired":  c.RegistrationCode != "" && c.OpenRegistration,
	}
}

package main

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

//go:embed static
var staticFS embed.FS

// realIP 取客户端真实 IP。挂 Cloudflare 等反代时开 trust_proxy，
// 从代理头还原真实 IP，否则限流会把所有用户算成一个
func realIP(r *http.Request, trust bool) string {
	if trust {
		if v := r.Header.Get("CF-Connecting-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	doGen := flag.Bool("gen-cert", false, "生成自签名证书 (cert.pem/key.pem) 后退出")
	extraHosts := flag.String("hosts", "", "自签名证书附加域名或 IP，逗号分隔")
	port := flag.Int("port", 0, "覆盖配置文件的监听端口，如 -port 6767")
	admin := flag.String("admin", "", "管理命令: list|stats|backup|delete|passwd|rename|setregcode|setreg")
	force := flag.Bool("force", false, "管理写操作忽略运行锁")
	svcMode := flag.String("svc", "", "以 Windows 服务方式运行（由服务管理器调用，值固定为 run）")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置: %v", err)
	}

	if *port > 0 {
		if *port < 1 || *port > 65535 {
			log.Fatal("端口范围 1-65535")
		}
		cfg.Addr = fmt.Sprintf(":%d", *port)
	}

	if *admin != "" {
		runAdmin(cfg, *cfgPath, *admin, flag.Args(), *force)
		return
	}

	if *svcMode == "run" {
		if err := runService(cfg); err != nil {
			log.Fatalf("服务运行失败: %v", err)
		}
		return
	}

	if *doGen {
		if err := genSelfSigned(*extraHosts); err != nil {
			log.Fatalf("生成证书: %v", err)
		}
		log.Println("已生成 cert.pem / key.pem (ECDSA P-256, 10 年)")
		return
	}

	srv, err := startServer(cfg)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	RemoveLockFile(cfg.DataDir)
}

// startServer 构建并启动监听，控制台与 Windows 服务模式共用。
// 监听失败同步返回错误；成功后服务在后台 goroutine 运行
func startServer(cfg *Config) (*http.Server, error) {
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	// 运行锁：提示管理命令不要在服务运行时写数据
	if err := WriteLockFile(cfg.DataDir); err != nil {
		return nil, err
	}
	hub := newHub(cfg, store)
	api := &apiServer{cfg: cfg, store: store, hub: hub}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.serveWS)
	mux.HandleFunc("POST /api/register", api.handleRegister)
	mux.HandleFunc("POST /api/login", api.handleLogin)
	mux.HandleFunc("POST /api/logout", api.handleLogout)
	mux.HandleFunc("GET /api/me", api.handleMe)
	mux.HandleFunc("GET /api/contacts", api.handleContacts)
	mux.HandleFunc("POST /api/contacts", api.handleContactAdd)
	mux.HandleFunc("DELETE /api/contacts/{vid}", api.handleContactDelete)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(cfg.clientConfig())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", noStore(http.FileServer(http.FS(sub))))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}

	tlsConf := buildTLSConfig(cfg)
	var ln net.Listener
	if tlsConf == nil {
		log.Printf("HTTP 监听 %s —— 明文模式，仅限本机调试或 Cloudflare Flexible 回源", cfg.Addr)
		ln, err = net.Listen("tcp", cfg.Addr)
	} else {
		log.Printf("HTTPS 监听 %s (仅 TLS 1.3)", cfg.Addr)
		ln, err = tls.Listen("tcp", cfg.Addr, tlsConf)
	}
	if err != nil {
		RemoveLockFile(cfg.DataDir)
		return nil, err
	}
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			log.Printf("服务异常退出: %v", serr)
		}
	}()
	return srv, nil
}

func buildTLSConfig(cfg *Config) *tls.Config {
	switch cfg.TLSMode {
	case "none":
		return nil
	case "acme":
		if cfg.Domain == "" {
			log.Fatal("tls_mode=acme 需要在配置中填写 domain")
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Cache:      autocert.DirCache(cfg.CertCache),
		}
		tc := m.TLSConfig()
		tc.MinVersion = tls.VersionTLS13
		return tc
	default: // manual
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			log.Fatal("tls_mode=manual 需要 cert_file/key_file，或先运行 -gen-cert 生成自签名证书")
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatalf("加载证书: %v (提示: 可先运行 -gen-cert 生成自签名证书)", err)
		}
		return &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		}
	}
}

// securityHeaders 全站安全响应头
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(self), microphone=(self), display-capture=(self), geolocation=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		csp := "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
			"media-src 'self' blob:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
		if r.TLS == nil {
			csp = strings.Replace(csp, "connect-src 'self'", "connect-src 'self' ws:", 1)
		} else {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// noStore 不缓存页面，降低共用电脑上的残留风险
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---------- 管理命令 ----------

// processAlive 判断持锁进程是否还活着（防强杀/崩溃后的残留锁把管理员锁在门外）
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false // Windows 下进程不存在会在这里报错
	}
	if runtime.GOOS == "windows" {
		return true // Windows 不支持 Signal(0)，能打开进程就视为活着
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// checkLock 服务运行中返回 true；锁是残留的则顺手清掉并返回 false。
// 判活以端口探测为准（服务活着必然在监听），PID 校验在非 Windows 上做双保险
func checkLock(cfg *Config) bool {
	dir := cfg.DataDir
	if !LockExists(dir) {
		return false
	}
	if cfg.Addr != "" {
		host := cfg.Addr
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		if conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond); err == nil {
			conn.Close()
			return true
		}
	}
	if runtime.GOOS != "windows" {
		if data, err := os.ReadFile(lockPath(dir)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && processAlive(pid) {
				return true
			}
		}
	}
	RemoveLockFile(dir)
	log.Println("检测到残留运行锁（持锁进程已退出），已自动清除")
	return false
}

func adminUsage() {
	fmt.Println(`管理命令用法 (注意: -config 等选项要写在 -admin 前面):
  peacefulVA-server -config config.json -admin list                    用户列表
  peacefulVA-server -admin stats                   统计（用户/好友对/会话数）
  peacefulVA-server -admin backup                  立即备份账号数据到 data/backups/
  peacefulVA-server -admin delete <用户名|VID>      删除账号（清理好友关系与会话）
  peacefulVA-server -admin passwd <用户名|VID> <新密码>   重置密码（至少8位）
  peacefulVA-server -admin rename <用户名|VID> <新昵称>   改昵称
  peacefulVA-server -admin setregcode <注册码|->    设置注册码，- 为清除（改 config.json）
  peacefulVA-server -admin setreg <open|close>      开/关注册（改 config.json）

说明:
  - 服务运行中会持有 data/.lock，写类命令会拒绝执行；先停服务或加 -force
  - setregcode/setreg 改完需重启服务生效`)
}

// writeConfigField 修改 config.json 单个字段，保留其余内容
func writeConfigField(path, key string, value any) error {
	var m map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	m[key] = value
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runAdmin(cfg *Config, cfgPath, cmd string, args []string, force bool) {
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	writeOps := map[string]bool{"delete": true, "passwd": true, "rename": true, "backup": true, "setregcode": true, "setreg": true}
	if writeOps[cmd] && checkLock(cfg) && !force {
		log.Fatalf("服务正在运行（存在 %s），写操作会冲突。先停服务再执行，或确认无碍后加 -force", lockPath(cfg.DataDir))
	}

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("打开账号数据: %v", err)
	}

	switch cmd {
	case "list":
		users := store.ListUsers()
		fmt.Printf("%-10s %-16s %-20s %-6s %s\n", "VID", "用户名", "昵称", "好友", "注册日期")
		for _, u := range users {
			fmt.Printf("%-10s %-16s %-20s %-6d %s\n",
				u["vid"], u["username"], u["name"], u["contacts"], u["created"])
		}
		fmt.Printf("共 %d 个账号\n", len(users))
	case "stats":
		st := store.Stats()
		fmt.Printf("账号 %d | 好友对 %d | 有效会话 %d\n", st["users"], st["friendPairs"], st["sessions"])
	case "backup":
		store.backupData(10)
		fmt.Println("已备份到", filepath.Join(cfg.DataDir, "backups"), "（保留最近 10 份）")
	case "delete":
		if len(args) != 1 {
			adminUsage()
			os.Exit(1)
		}
		if err := store.DeleteAccount(args[0]); err != nil {
			log.Fatalf("删除失败: %v", err)
		}
		fmt.Printf("已删除 %s（好友关系与会话一并清理）\n", args[0])
	case "passwd":
		if len(args) != 2 {
			adminUsage()
			os.Exit(1)
		}
		if err := store.SetPassword(args[0], args[1]); err != nil {
			log.Fatalf("重置失败: %v", err)
		}
		fmt.Printf("已重置 %s 的密码\n", args[0])
	case "rename":
		if len(args) != 2 {
			adminUsage()
			os.Exit(1)
		}
		if err := store.Rename(args[0], args[1]); err != nil {
			log.Fatalf("改名失败: %v", err)
		}
		fmt.Printf("已改名 %s -> %s\n", args[0], args[1])
	case "setregcode":
		if len(args) != 1 {
			adminUsage()
			os.Exit(1)
		}
		code := args[0]
		if code == "-" {
			code = ""
		}
		if err := writeConfigField(cfgPath, "registration_code", code); err != nil {
			log.Fatalf("写入配置失败: %v", err)
		}
		fmt.Println("已更新 registration_code，重启服务生效")
	case "setreg":
		if len(args) != 1 || (args[0] != "open" && args[0] != "close") {
			adminUsage()
			os.Exit(1)
		}
		if err := writeConfigField(cfgPath, "open_registration", args[0] == "open"); err != nil {
			log.Fatalf("写入配置失败: %v", err)
		}
		fmt.Println("已更新 open_registration，重启服务生效")
	default:
		adminUsage()
		os.Exit(1)
	}
}

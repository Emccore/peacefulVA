package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

const (
	writeWait     = 10 * time.Second
	pongWait      = 60 * time.Second
	pingPeriod    = 45 * time.Second
	maxMsgSize    = 32 * 1024
	maxSignalLen  = 24 * 1024
	maxChatLen    = 24 * 1024
	sendBufSize   = 256
	msgRatePerSec = 20
	msgBurst      = 40
)

// inboundMsg 客户端上行消息
// ready: 上线并公布本次会话的 ECDH 公钥
// sig: WebRTC 协商数据，原样转发
// chat: 端到端加密文字，服务器只转密文
// call/accept/decline/cancel/hangup: 呼叫控制
type inboundMsg struct {
	Type  string          `json:"type"`
	To    string          `json:"to"`
	Pub   string          `json:"pub"`
	Data  json.RawMessage `json:"data"`
	Iv    string          `json:"iv"`
	Ct    string          `json:"ct"`
	Media string          `json:"media"`
}

type Client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	vid     string
	name    string
	pub     string
	limiter *rate.Limiter
	drops   int

	sendMu sync.RWMutex
	closed bool
}

// Hub 在线连接表 + 信令转发。媒体不经过这里
type Hub struct {
	cfg      *Config
	store    *Store
	upgrader websocket.Upgrader

	mu    sync.Mutex
	conns map[string]*Client // vid -> conn（一人一连接，重复登录顶掉旧连接）

	ipMu   sync.Mutex
	ips    map[string]*rate.Limiter
	ipSeen map[string]time.Time
	wsByIP map[string]int // 每 IP 在线连接数，防连接洪泛
}

func newHub(cfg *Config, store *Store) *Hub {
	h := &Hub{
		cfg:    cfg,
		store:  store,
		conns:  map[string]*Client{},
		ips:    map[string]*rate.Limiter{},
		ipSeen: map[string]time.Time{},
		wsByIP: map[string]int{},
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 只接受同源页面发起的 WebSocket（经 Cloudflare 等反代时 Host 头不变，依然成立）
		CheckOrigin: func(r *http.Request) bool {
			o := r.Header.Get("Origin")
			if o == "" {
				return false
			}
			u, err := url.Parse(o)
			if err != nil {
				return false
			}
			return u.Host == r.Host
		},
	}
	go h.cleanupIPs()
	return h
}

// limitIP 按 IP 限连接频率
func (h *Hub) limitIP(ip string) bool {
	return h.limitKey(ip, rate.Every(500*time.Millisecond), 8)
}

// limitAuth 登录/注册专用，更严格，防暴力试密码
func (h *Hub) limitAuth(ip string) bool {
	return h.limitKey("auth|"+ip, rate.Every(time.Second), 5)
}

func (h *Hub) limitKey(key string, every rate.Limit, burst int) bool {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	lim, ok := h.ips[key]
	if !ok {
		lim = rate.NewLimiter(every, burst)
		h.ips[key] = lim
	}
	h.ipSeen[key] = time.Now()
	return lim.Allow()
}

func (h *Hub) cleanupIPs() {
	t := time.NewTicker(10 * time.Minute)
	for range t.C {
		h.ipMu.Lock()
		for ip, ts := range h.ipSeen {
			if time.Since(ts) > 30*time.Minute {
				delete(h.ipSeen, ip)
				delete(h.ips, ip)
			}
		}
		h.ipMu.Unlock()
	}
}

// onlineInfo 查某人是否在线及其公钥
func (h *Hub) onlineInfo(vid string) (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.conns[vid]
	if !ok {
		return false, ""
	}
	return true, c.pub
}

// notifyContactRemoved 好友关系被解除时通知在线一方
func (h *Hub) notifyContactRemoved(byVID, targetVID string) {
	h.mu.Lock()
	target := h.conns[targetVID]
	h.mu.Unlock()
	if target != nil {
		target.sendJSON(map[string]any{"t": "contact-removed", "vid": byVID})
	}
}

// incConns 单 IP 并发连接上限。同一账号顶号重连不受限，避免把自己锁在门外
func (h *Hub) incConns(ip, vid string) bool {
	limit := h.cfg.MaxConnsPerIP
	if limit <= 0 {
		limit = 10
	}
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if h.wsByIP[ip] >= limit {
		h.mu.Lock()
		_, takeover := h.conns[vid]
		h.mu.Unlock()
		if !takeover {
			return false
		}
	}
	h.wsByIP[ip]++
	return true
}

func (h *Hub) decConns(ip string) {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if h.wsByIP[ip] <= 1 {
		delete(h.wsByIP, ip)
	} else {
		h.wsByIP[ip]--
	}
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r, h.cfg.TrustProxy)
	if !h.limitIP(ip) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	// 会话校验：握手时浏览器自动带上登录 Cookie
	u := h.store.userFromRequest(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.incConns(ip, u.VID) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer h.decConns(ip)
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Client{
		hub:     h,
		conn:    conn,
		send:    make(chan []byte, sendBufSize),
		vid:     u.VID,
		name:    u.Name,
		limiter: rate.NewLimiter(msgRatePerSec, msgBurst),
	}

	h.mu.Lock()
	old := h.conns[u.VID]
	h.conns[u.VID] = c
	h.mu.Unlock()
	if old != nil {
		// 先发关闭帧再断开，客户端据此提示"已在别处登录"而不是瞎重连
		_ = old.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(4001, "账号在其他窗口登录"),
			time.Now().Add(writeWait))
		old.kill()
	}

	if h.cfg.LogIPs {
		log.Printf("[ws] %s(%s) 上线 from %s", u.Name, u.VID, ip)
	} else {
		log.Printf("[ws] %s 上线", u.VID)
	}
	go c.writePump()
	c.readPump()
}

func (c *Client) readPump() {
	defer c.cleanup()
	conn := c.conn
	conn.SetReadLimit(maxMsgSize)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if !c.limiter.Allow() {
			continue
		}
		var m inboundMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.handleMsg(&m)
	}
}

func (c *Client) writePump() {
	conn := c.conn
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) handleMsg(m *inboundMsg) {
	switch m.Type {
	case "ready":
		c.onReady(m)
	case "sig":
		c.relay("sig", m, m.Data != nil && len(m.Data) <= maxSignalLen)
	case "chat":
		c.relay("chat", m, m.Ct != "" && len(m.Ct) <= maxChatLen && len(m.Iv) <= 32)
	case "call":
		c.relay("call", m, m.Media == "video" || m.Media == "voice")
	case "accept", "decline", "cancel", "hangup":
		c.relay(m.Type, m, true)
	case "leave":
		// 客户端随即关闭连接
	default:
		c.sendErr("未知消息类型")
	}
}

// onReady 上线：回快照，并把自己的公钥广播给在线好友
func (c *Client) onReady(m *inboundMsg) {
	if len(m.Pub) > 400 {
		c.sendErr("公钥格式不对")
		return
	}
	c.pub = m.Pub

	contacts := c.hub.store.Contacts(c.vid)
	type item struct {
		Vid    string `json:"vid"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
		Pub    string `json:"pub,omitempty"`
	}
	list := make([]item, 0, len(contacts))
	var onlinePeers []*Client

	c.hub.mu.Lock()
	for _, ct := range contacts {
		it := item{Vid: ct.VID, Name: ct.Name}
		if peer, ok := c.hub.conns[ct.VID]; ok {
			it.Online = true
			it.Pub = peer.pub
			onlinePeers = append(onlinePeers, peer)
		}
		list = append(list, it)
	}
	c.hub.mu.Unlock()

	c.sendJSON(map[string]any{"t": "hello", "you": map[string]string{"vid": c.vid, "name": c.name}})
	c.sendJSON(map[string]any{"t": "contacts", "list": list})
	for _, p := range onlinePeers {
		p.sendJSON(map[string]any{"t": "presence", "vid": c.vid, "online": true, "pub": c.pub})
	}
}

// relay 转发给指定好友。要求对方在自己的好友列表里
func (c *Client) relay(kind string, m *inboundMsg, payloadOK bool) {
	if !payloadOK {
		return
	}
	to := normalizeVid(m.To)
	if len(to) != vidLen {
		return
	}
	if !c.hub.store.HasContact(c.vid, to) {
		c.sendErr("只能联系自己的好友")
		return
	}
	c.hub.mu.Lock()
	target := c.hub.conns[to]
	c.hub.mu.Unlock()
	if target == nil {
		c.sendErr("对方不在线")
		return
	}
	msg := map[string]any{"t": kind, "from": c.vid}
	switch kind {
	case "sig":
		msg["d"] = m.Data
	case "chat":
		msg["iv"] = m.Iv
		msg["ct"] = m.Ct
	case "call":
		msg["media"] = m.Media
	}
	target.sendJSON(msg)
}

func (c *Client) cleanup() {
	h := c.hub
	h.mu.Lock()
	var notified []*Client
	if h.conns[c.vid] == c {
		delete(h.conns, c.vid)
		// 只有当前连接真正下线才广播离线；被新连接顶掉时不广播
		for _, ct := range h.store.Contacts(c.vid) {
			if peer, ok := h.conns[ct.VID]; ok {
				notified = append(notified, peer)
			}
		}
	}
	h.mu.Unlock()
	for _, p := range notified {
		p.sendJSON(map[string]any{"t": "presence", "vid": c.vid, "online": false})
	}
	c.kill()
	log.Printf("[ws] %s 离线", c.vid)
}

func (c *Client) sendJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.sendMu.RLock()
	overflow := false
	if !c.closed {
		select {
		case c.send <- b:
		default:
			c.drops++
			overflow = c.drops > 200
		}
	}
	c.sendMu.RUnlock()
	if overflow {
		c.kill()
	}
}

func (c *Client) sendErr(msg string) {
	c.sendJSON(map[string]any{"t": "err", "msg": msg})
}

func (c *Client) kill() {
	c.sendMu.Lock()
	if !c.closed {
		c.closed = true
		close(c.send)
	}
	c.sendMu.Unlock()
	_ = c.conn.Close()
}

func normalizeVid(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

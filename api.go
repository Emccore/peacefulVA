package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type apiServer struct {
	cfg   *Config
	store *Store
	hub   *Hub
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, 400, "请求格式错误")
		return false
	}
	return true
}

func (a *apiServer) meInfo(u *User) map[string]any {
	return map[string]any{"vid": u.VID, "username": u.Username, "name": u.Name}
}

// POST /api/register
func (a *apiServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !a.hub.limitAuth(realIP(r, a.cfg.TrustProxy)) {
		fail(w, 429, "请求太频繁，稍后再试")
		return
	}
	if !a.cfg.OpenRegistration {
		fail(w, 403, "本站未开放注册")
		return
	}
	var in struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if !checkRegistrationCode(in.Code, a.cfg.RegistrationCode) {
		fail(w, 403, "注册码错误")
		return
	}
	u, err := a.store.CreateUser(in.Username, in.Name, in.Password)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	token, err := a.store.CreateSession(u.VID)
	if err != nil {
		fail(w, 500, "会话创建失败")
		return
	}
	setSessionCookie(w, token, a.cookieSecure(), sessionDays*24*3600)
	writeJSON(w, 200, a.meInfo(u))
}

// POST /api/login
func (a *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.hub.limitAuth(realIP(r, a.cfg.TrustProxy)) {
		fail(w, 429, "请求太频繁，稍后再试")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readBody(w, r, &in) {
		return
	}
	u, err := a.store.VerifyLogin(in.Username, in.Password)
	if err != nil {
		fail(w, 401, "账号或密码不对")
		return
	}
	token, err := a.store.CreateSession(u.VID)
	if err != nil {
		fail(w, 500, "会话创建失败")
		return
	}
	setSessionCookie(w, token, a.cookieSecure(), sessionDays*24*3600)
	writeJSON(w, 200, a.meInfo(u))
}

// POST /api/logout
func (a *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteSession(ck.Value)
	}
	setSessionCookie(w, "", a.cookieSecure(), -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// GET /api/me
func (a *apiServer) handleMe(w http.ResponseWriter, r *http.Request) {
	u := a.store.userFromRequest(r)
	if u == nil {
		fail(w, 401, "未登录")
		return
	}
	writeJSON(w, 200, a.meInfo(u))
}

// GET /api/contacts
func (a *apiServer) handleContacts(w http.ResponseWriter, r *http.Request) {
	u := a.store.userFromRequest(r)
	if u == nil {
		fail(w, 401, "未登录")
		return
	}
	list := a.store.Contacts(u.VID)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		online, pub := a.hub.onlineInfo(c.VID)
		item := map[string]any{"vid": c.VID, "name": c.Name, "online": online}
		if online {
			item["pub"] = pub
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"list": out})
}

// POST /api/contacts {vid}
func (a *apiServer) handleContactAdd(w http.ResponseWriter, r *http.Request) {
	u := a.store.userFromRequest(r)
	if u == nil {
		fail(w, 401, "未登录")
		return
	}
	var in struct {
		Vid string `json:"vid"`
	}
	if !readBody(w, r, &in) {
		return
	}
	vid := strings.ToLower(strings.TrimSpace(in.Vid))
	if len(vid) != vidLen {
		fail(w, 400, "好友 ID 格式不对")
		return
	}
	c, err := a.store.AddContact(u.VID, vid)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			fail(w, 404, "没有这个用户")
			return
		}
		fail(w, 400, err.Error())
		return
	}
	online, _ := a.hub.onlineInfo(c.VID)
	writeJSON(w, 200, map[string]any{"vid": c.VID, "name": c.Name, "online": online})
}

// DELETE /api/contacts/{vid}
func (a *apiServer) handleContactDelete(w http.ResponseWriter, r *http.Request) {
	u := a.store.userFromRequest(r)
	if u == nil {
		fail(w, 401, "未登录")
		return
	}
	vid := r.PathValue("vid")
	if err := a.store.RemoveContact(u.VID, vid); err != nil {
		fail(w, 400, err.Error())
		return
	}
	// 通知在线的对方关系已解除
	a.hub.notifyContactRemoved(u.VID, vid)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *apiServer) cookieSecure() bool {
	if a.cfg.SecureCookies {
		return true
	}
	return a.cfg.TLSMode != "none"
}

package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUserExists  = errors.New("用户名已被占用")
	ErrUserNotFound = errors.New("用户不存在")
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_-]{3,24}$`)

// vid 字符集：去掉易混淆的 0/o/1/l/i
const vidAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"
const vidLen = 8

// User 账号。VID 是系统分配的全局唯一 ID，好友添加和呼叫都靠它
type User struct {
	VID      string    `json:"vid"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	Contacts []string  `json:"contacts"`
}

type sessionRec struct {
	VID string `json:"vid"`
	Exp int64  `json:"exp"`
}

// Store 账号与会话存储。小规模自部署场景用 JSON 文件，
// 写入走临时文件+改名，避免半截数据
type Store struct {
	mu       sync.RWMutex
	dir      string
	users    map[string]*User // vid -> user
	byName   map[string]string
	sessions map[string]*sessionRec
}

const sessionDays = 30

func OpenStore(dir string) (*Store, error) {
	s := &Store{
		dir:      dir,
		users:    map[string]*User{},
		byName:   map[string]string{},
		sessions: map[string]*sessionRec{},
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var users []*User
	if err := s.loadJSON("users.json", &users); err != nil {
		return nil, err
	}
	for _, u := range users {
		s.users[u.VID] = u
		s.byName[u.Username] = u.VID
	}
	if err := s.loadJSON("sessions.json", &s.sessions); err != nil {
		return nil, err
	}
	// 已有数据则滚动备份，防止升级/误操作导致账号丢失
	if _, err := os.Stat(filepath.Join(dir, "users.json")); err == nil {
		s.backupData(7)
	}
	// 清掉过期会话
	now := time.Now().Unix()
	changed := false
	for tk, se := range s.sessions {
		if se.Exp < now {
			delete(s.sessions, tk)
			changed = true
		}
	}
	if changed {
		if err := s.saveJSON("sessions.json", s.sessions); err != nil {
			return nil, err
		}
	}
	go s.sessionCleaner()
	return s, nil
}

// sessionCleaner 定期清理过期会话，避免长期运行时 sessions.json 膨胀
func (s *Store) sessionCleaner() {
	t := time.NewTicker(time.Hour)
	for range t.C {
		now := time.Now().Unix()
		s.mu.Lock()
		changed := false
		for tk, se := range s.sessions {
			if se.Exp < now {
				delete(s.sessions, tk)
				changed = true
			}
		}
		if changed {
			_ = s.saveSessionsLocked()
		}
		s.mu.Unlock()
	}
}

func (s *Store) loadJSON(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (s *Store) saveJSON(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) saveUsersLocked() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	return s.saveJSON("users.json", list)
}

func (s *Store) saveSessionsLocked() error {
	return s.saveJSON("sessions.json", s.sessions)
}

func genVID() string {
	b := make([]byte, vidLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, vidLen)
	for i, v := range b {
		out[i] = vidAlphabet[int(v)%len(vidAlphabet)]
	}
	return string(out)
}

func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	r := []rune(s)
	if len(r) > 32 {
		r = r[:32]
	}
	s = strings.TrimSpace(string(r))
	if s == "" {
		return "用户"
	}
	return s
}

// CreateUser 注册新账号
func (s *Store) CreateUser(username, name, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return nil, errors.New("账号需为 3-24 位小写字母、数字、- 或 _")
	}
	if len(password) < 8 || len(password) > 72 {
		return nil, errors.New("密码至少 8 位")
	}
	name = sanitizeName(name)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byName[username]; exists {
		return nil, ErrUserExists
	}
	u := &User{
		VID:      genVID(),
		Username: username,
		Name:     name,
		Hash:     string(hash),
		Created:  time.Now(),
	}
	// VID 碰撞重试
	for {
		if _, clash := s.users[u.VID]; !clash {
			break
		}
		u.VID = genVID()
	}
	s.users[u.VID] = u
	s.byName[username] = u.VID
	if err := s.saveUsersLocked(); err != nil {
		delete(s.users, u.VID)
		delete(s.byName, username)
		return nil, err
	}
	return u.copy(), nil
}

// VerifyLogin 校验账密
func (s *Store) VerifyLogin(username, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	s.mu.RLock()
	vid, ok := s.byName[username]
	var u *User
	if ok {
		u = s.users[vid]
	}
	hash := ""
	if u != nil {
		hash = u.Hash
	}
	s.mu.RUnlock()
	if u == nil {
		// 仍执行一次哈希对比，避免用户名枚举的时间差
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0X8fF0WqUcFE0k4VfPq6OzUfO0O"), []byte(password))
		return nil, ErrUserNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrUserNotFound
	}
	return u.copy(), nil
}

// copy 返回用户快照，避免外部直接改内部数据
func (u *User) copy() *User {
	c := *u
	c.Contacts = append([]string(nil), u.Contacts...)
	return &c
}

// UserByVID 查用户（只读快照）
func (s *Store) UserByVID(vid string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u, ok := s.users[vid]; ok {
		return u.copy()
	}
	return nil
}

// AddContact 我的好友列表里加一个人
func (s *Store) AddContact(vid, contactVID string) (*User, error) {
	if vid == contactVID {
		return nil, errors.New("不能添加自己")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.users[contactVID]
	if !ok {
		return nil, ErrUserNotFound
	}
	me, ok := s.users[vid]
	if !ok {
		return nil, ErrUserNotFound
	}
	for _, c := range me.Contacts {
		if c == contactVID {
			return target.copy(), nil // 已存在，幂等
		}
	}
	if len(me.Contacts) >= 200 {
		return nil, errors.New("好友数量已达上限")
	}
	me.Contacts = append(me.Contacts, contactVID)
	if err := s.saveUsersLocked(); err != nil {
		me.Contacts = me.Contacts[:len(me.Contacts)-1]
		return nil, err
	}
	return target.copy(), nil
}

// RemoveContact 删除好友：双向解除关系
func (s *Store) RemoveContact(vid, contactVID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	me, ok := s.users[vid]
	if !ok {
		return ErrUserNotFound
	}
	idx := -1
	for i, c := range me.Contacts {
		if c == contactVID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil // 本来就不在列表里，幂等
	}
	oldMine := append([]string(nil), me.Contacts...)
	me.Contacts = append(me.Contacts[:idx], me.Contacts[idx+1:]...)

	other, hasOther := s.users[contactVID]
	var oldTheirs []string
	if hasOther {
		oldTheirs = append([]string(nil), other.Contacts...)
		for i, c := range other.Contacts {
			if c == vid {
				other.Contacts = append(other.Contacts[:i], other.Contacts[i+1:]...)
				break
			}
		}
	}
	if err := s.saveUsersLocked(); err != nil {
		me.Contacts = oldMine
		if hasOther {
			other.Contacts = oldTheirs
		}
		return err
	}
	return nil
}

// Contacts 返回我的好友列表（快照）
func (s *Store) Contacts(vid string) []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	me, ok := s.users[vid]
	if !ok {
		return nil
	}
	out := make([]*User, 0, len(me.Contacts))
	for _, c := range me.Contacts {
		if u, ok := s.users[c]; ok {
			out = append(out, u.copy())
		}
	}
	return out
}

func (s *Store) HasContact(vid, contactVID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	me, ok := s.users[vid]
	if !ok {
		return false
	}
	for _, c := range me.Contacts {
		if c == contactVID {
			return true
		}
	}
	return false
}

// CreateSession 签发会话，返回 token
func (s *Store) CreateSession(vid string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = &sessionRec{VID: vid, Exp: time.Now().Add(sessionDays * 24 * time.Hour).Unix()}
	if err := s.saveSessionsLocked(); err != nil {
		delete(s.sessions, token)
		return "", err
	}
	return token, nil
}

func (s *Store) DeleteSession(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[token]; ok {
		delete(s.sessions, token)
		_ = s.saveSessionsLocked()
	}
}

// UserByToken 校验会话并返回用户快照
func (s *Store) UserByToken(token string) *User {
	if token == "" {
		return nil
	}
	s.mu.RLock()
	se, ok := s.sessions[token]
	vid := ""
	if ok {
		if se.Exp < time.Now().Unix() {
			ok = false
		} else {
			vid = se.VID
		}
	}
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return s.UserByVID(vid)
}

const sessionCookie = "pva_session"

func setSessionCookie(w http.ResponseWriter, token string, secure bool, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

func (s *Store) userFromRequest(r *http.Request) *User {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return s.UserByToken(ck.Value)
}

// checkRegistrationCode 注册码校验（常量时间比较）
func checkRegistrationCode(input, expect string) bool {
	if expect == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(input), []byte(expect)) == 1
}

// ---------- 数据保留与运维 ----------

func lockPath(dir string) string { return filepath.Join(dir, ".lock") }

// WriteLockFile 服务运行期间持有；管理命令写数据前检查，防止双写冲突
func WriteLockFile(dir string) error {
	return os.WriteFile(lockPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func RemoveLockFile(dir string) { _ = os.Remove(lockPath(dir)) }

func LockExists(dir string) bool {
	_, err := os.Stat(lockPath(dir))
	return err == nil
}

// backupData 滚动备份 users/sessions 到 data/backups/<时间戳>/，保留最近 keep 份
func (s *Store) backupData(keep int) {
	bdir := filepath.Join(s.dir, "backups")
	_ = os.MkdirAll(bdir, 0o700)
	bk := filepath.Join(bdir, time.Now().Format("20060102-150405"))
	_ = os.MkdirAll(bk, 0o700)
	for _, f := range []string{"users.json", "sessions.json"} {
		data, err := os.ReadFile(filepath.Join(s.dir, f))
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(bk, f), data, 0o600)
	}
	ents, err := os.ReadDir(bdir)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs) // 时间戳命名，字典序即时间序
	for len(dirs) > keep {
		_ = os.RemoveAll(filepath.Join(bdir, dirs[0]))
		dirs = dirs[1:]
	}
}

// resolveLocked 锁内按用户名或 VID 找用户
func (s *Store) resolveLocked(idOrName string) *User {
	idOrName = strings.ToLower(strings.TrimSpace(idOrName))
	if vid, ok := s.byName[idOrName]; ok {
		return s.users[vid]
	}
	if u, ok := s.users[idOrName]; ok {
		return u
	}
	return nil
}

func (s *Store) ListUsers() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, map[string]any{
			"vid": u.VID, "username": u.Username, "name": u.Name,
			"contacts": len(u.Contacts), "created": u.Created.Format("2006-01-02"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["created"].(string) < out[j]["created"].(string)
	})
	return out
}

func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	edges := 0
	for _, u := range s.users {
		edges += len(u.Contacts)
	}
	return map[string]int{"users": len(s.users), "friendPairs": edges / 2, "sessions": len(s.sessions)}
}

// DeleteAccount 删除账号并清理其所有好友关系和会话
func (s *Store) DeleteAccount(idOrName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.resolveLocked(idOrName)
	if u == nil {
		return ErrUserNotFound
	}
	vid := u.VID
	delete(s.users, vid)
	delete(s.byName, u.Username)
	for _, other := range s.users {
		for i, c := range other.Contacts {
			if c == vid {
				other.Contacts = append(other.Contacts[:i], other.Contacts[i+1:]...)
				break
			}
		}
	}
	for tk, se := range s.sessions {
		if se.VID == vid {
			delete(s.sessions, tk)
		}
	}
	if err := s.saveUsersLocked(); err != nil {
		return err
	}
	return s.saveSessionsLocked()
}

func (s *Store) SetPassword(idOrName, password string) error {
	if len(password) < 8 || len(password) > 72 {
		return errors.New("密码至少 8 位")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.resolveLocked(idOrName)
	if u == nil {
		return ErrUserNotFound
	}
	u.Hash = string(hash)
	return s.saveUsersLocked()
}

func (s *Store) Rename(idOrName, name string) error {
	name = sanitizeName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.resolveLocked(idOrName)
	if u == nil {
		return ErrUserNotFound
	}
	u.Name = name
	return s.saveUsersLocked()
}

// Package auth 实现最简单直接的单管理员账户认证：
//   - 首次使用需要创建一个管理账户（用户名+密码），创建后不允许重复创建；
//   - 密码用随机盐 + HMAC-SHA256 多轮迭代存储（只用标准库，不引入第三方加密库）；
//   - 登录成功后签发一个随机会话令牌，保存在服务进程内存中（重启后需要重新登录，
//     对本地个人使用的交易机器人来说这是更安全的默认行为，而不是负担）。
//
// 这不是一套面向多用户、多角色的企业级认证系统，只解决"没有账号密码不能
// 打开控制台"这一个具体需求。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"gridbot/store"
)

const (
	kdfIterations = 100000
	sessionTTL    = 7 * 24 * time.Hour
)

// Manager 管理账户创建、登录校验与会话
type Manager struct {
	st *store.Store

	mu       sync.RWMutex
	sessions map[string]time.Time // token -> 过期时间
}

// New 创建认证管理器
func New(st *store.Store) *Manager {
	return &Manager{st: st, sessions: map[string]time.Time{}}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashPassword 用随机盐对密码做多轮 HMAC-SHA256 迭代，抵御彩虹表和暴力破解。
// 这是标准库能提供的最简单可靠的做法（不引入 golang.org/x/crypto/bcrypt 之类
// 的额外依赖，避免重蹈之前 sqlite 驱动那样的联网下载麻烦）。
func hashPassword(password, saltHex string) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(salt, []byte(password)...))
	cur := sum[:]
	for i := 0; i < kdfIterations; i++ {
		mac := hmac.New(sha256.New, salt)
		mac.Write(cur)
		cur = mac.Sum(nil)
	}
	return hex.EncodeToString(cur), nil
}

// HasAccount 判断是否已创建过管理账户
func (m *Manager) HasAccount() (bool, error) {
	return m.st.HasAdminAccount()
}

// CreateAccount 创建初始管理账户；如果已存在账户则拒绝（防止后续被未授权覆盖）
func (m *Manager) CreateAccount(username, password string) error {
	if len(username) == 0 {
		return errors.New("用户名不能为空")
	}
	if len(password) < 6 {
		return errors.New("密码至少需要6位")
	}
	has, err := m.st.HasAdminAccount()
	if err != nil {
		return err
	}
	if has {
		return errors.New("管理账户已存在，不能重复创建；如需修改请使用修改密码功能")
	}
	salt, err := randomHex(16)
	if err != nil {
		return err
	}
	hash, err := hashPassword(password, salt)
	if err != nil {
		return err
	}
	return m.st.CreateAdminAccount(username, hash, salt)
}

// VerifyLogin 校验用户名密码是否正确
func (m *Manager) VerifyLogin(username, password string) (bool, error) {
	storedUsername, hash, salt, err := m.st.GetAdminAccount()
	if err != nil {
		return false, err
	}
	if storedUsername == "" {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(storedUsername)) != 1 {
		return false, nil
	}
	calc, err := hashPassword(password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(calc), []byte(hash)) == 1, nil
}

// ChangePassword 修改密码（需要提供正确的旧密码）
func (m *Manager) ChangePassword(oldPassword, newPassword string) error {
	username, _, _, err := m.st.GetAdminAccount()
	if err != nil {
		return err
	}
	if username == "" {
		return errors.New("尚未创建管理账户")
	}
	ok, err := m.VerifyLogin(username, oldPassword)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("原密码不正确")
	}
	if len(newPassword) < 6 {
		return errors.New("新密码至少需要6位")
	}
	salt, err := randomHex(16)
	if err != nil {
		return err
	}
	hash, err := hashPassword(newPassword, salt)
	if err != nil {
		return err
	}
	return m.st.UpdateAdminPassword(hash, salt)
}

// NewSession 签发一个新的会话令牌，有效期 7 天
func (m *Manager) NewSession() (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.sessions[token] = time.Now().Add(sessionTTL)
	m.mu.Unlock()
	return token, nil
}

// ValidSession 校验会话令牌是否有效且未过期
func (m *Manager) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	m.mu.RLock()
	exp, ok := m.sessions[token]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		m.mu.Lock()
		delete(m.sessions, token)
		m.mu.Unlock()
		return false
	}
	return true
}

// Logout 使某个会话令牌立即失效
func (m *Manager) Logout(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

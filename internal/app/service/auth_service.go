package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"nas-agent/internal/app/dao"
)

// ErrInvalidCredential 用户名或密码错误
var ErrInvalidCredential = errors.New("用户名或密码错误")

const tokenTTL = 24 * time.Hour

type session struct {
	UserID   int64
	Username string
	IsAdmin  bool
	ExpireAt time.Time
}

// AuthService 登录态管理（内存 token 表，重启后需重新登录）
type AuthService struct {
	mu     sync.RWMutex
	tokens map[string]session
}

// NewAuthService 创建认证服务
func NewAuthService() *AuthService {
	return &AuthService{tokens: make(map[string]session)}
}

// Login 校验账号密码并颁发 token
func (s *AuthService) Login(username, password string) (string, bool, error) {
	u, err := dao.GetUserByUsername(username)
	if err != nil {
		return "", false, ErrInvalidCredential
	}
	if u.PasswordHash != dao.HashPassword(password) {
		return "", false, ErrInvalidCredential
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("gen token: %w", err)
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[token] = session{UserID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin, ExpireAt: time.Now().Add(tokenTTL)}
	s.mu.Unlock()
	return token, u.IsAdmin, nil
}

// Logout 注销
func (s *AuthService) Logout(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// Validate 校验 token，返回会话信息（用户名、用户 ID、是否管理员）
func (s *AuthService) Validate(token string) (string, int64, bool, bool) {
	s.mu.RLock()
	sess, ok := s.tokens[token]
	s.mu.RUnlock()
	if !ok {
		return "", 0, false, false
	}
	if time.Now().After(sess.ExpireAt) {
		s.Logout(token)
		return "", 0, false, false
	}
	return sess.Username, sess.UserID, sess.IsAdmin, true
}

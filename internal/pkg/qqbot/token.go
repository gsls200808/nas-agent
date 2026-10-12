package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// tokenEndpoint QQ 官方 access_token 获取端点。
// 注意：官方 wiki 文档写 form-urlencoded，但官方 Go SDK（botgo）实际用 JSON body
// （{"appId","clientSecret"}，Content-Type: application/json），实测仅 JSON 可用。
// 用 var 以便单测注入 httptest 服务器。
var tokenEndpoint = "https://bots.qq.com/app/getAppAccessToken"

// tokenRefreshAhead token 到期前提前刷新的余量
const tokenRefreshAhead = 5 * time.Minute

// tokenManager AppID/Secret → access_token 缓存与自动刷新
type tokenManager struct {
	appID        string
	clientSecret string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenManager(appID, clientSecret string) *tokenManager {
	return &tokenManager{appID: appID, clientSecret: clientSecret}
}

// Token 返回有效 access_token，过期前 5 分钟自动刷新（并发安全）
func (m *tokenManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && time.Now().Before(m.expires.Add(-tokenRefreshAhead)) {
		return m.token, nil
	}
	// 与官方 botgo SDK 一致：JSON body
	payload, err := json.Marshal(struct {
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
	}{m.appID, m.clientSecret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("qqbot: get access token: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Token   string `json:"access_token"`
		Expires string `json:"expires_in"` // QQ 返回字符串形式的秒数
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("qqbot: decode token response: %w", err)
	}
	if body.Token == "" {
		return "", fmt.Errorf("qqbot: get access token failed: code=%d message=%s", body.Code, body.Message)
	}
	secs := 7200
	_, _ = fmt.Sscanf(body.Expires, "%d", &secs)
	m.token = body.Token
	m.expires = time.Now().Add(time.Duration(secs) * time.Second)
	return m.token, nil
}

// invalidate 主动失效缓存（收到 401/鉴权错误时调用）
func (m *tokenManager) invalidate() {
	m.mu.Lock()
	m.token = ""
	m.mu.Unlock()
}

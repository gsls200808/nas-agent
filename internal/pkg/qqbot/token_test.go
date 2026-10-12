package qqbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestTokenFetchAndCache(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json (botgo 兼容)", ct)
		}
		var form struct {
			AppID        string `json:"appId"`
			ClientSecret string `json:"clientSecret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
			t.Errorf("decode json body: %v", err)
		}
		if form.AppID != "app1" || form.ClientSecret != "sec1" {
			t.Errorf("body mismatch: %+v", form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "tok-1",
			"expires_in":   "7200", // QQ 返回字符串形式
		})
	}))
	defer srv.Close()

	old := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = old }()

	m := newTokenManager("app1", "sec1")
	ctx := context.Background()
	tok, err := m.Token(ctx)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("token mismatch: %q", tok)
	}
	// 缓存命中：不再请求
	if _, err = m.Token(ctx); err != nil {
		t.Fatalf("cached token: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expect 1 request, got %d", n)
	}
	// invalidate 后重新拉取
	m.invalidate()
	if _, err = m.Token(ctx); err != nil {
		t.Fatalf("refresh token: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("expect 2 requests, got %d", n)
	}
}

func TestTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "10003", "message": "invalid secret"})
	}))
	defer srv.Close()
	old := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = old }()

	m := newTokenManager("app1", "bad")
	if _, err := m.Token(context.Background()); err == nil {
		t.Fatalf("expect error for empty token")
	}
}

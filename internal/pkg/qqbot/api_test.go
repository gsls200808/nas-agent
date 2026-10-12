package qqbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessagesPath(t *testing.T) {
	cases := []struct {
		chat    ChatTarget
		suffix  string
		want    string
	}{
		{ChatTarget{Kind: "c2c", OpenID: "u1"}, "messages", "/v2/users/u1/messages"},
		{ChatTarget{Kind: "group", OpenID: "g1"}, "messages", "/v2/groups/g1/messages"},
		{ChatTarget{Kind: "c2c", OpenID: "u1"}, "files", "/v2/users/u1/files"},
		{ChatTarget{Kind: "group", OpenID: "g1"}, "files", "/v2/groups/g1/files"},
	}
	for _, c := range cases {
		if got := messagesPath(c.chat, c.suffix); got != c.want {
			t.Errorf("messagesPath(%+v, %s) = %s, want %s", c.chat, c.suffix, got, c.want)
		}
	}
}

// TestRequestAuthAndError 校验鉴权头与错误解析
func TestRequestAuthAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "QQBot tok-1" {
			t.Errorf("auth header = %q", got)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 123, "message": "bad request"})
	}))
	defer srv.Close()

	c := &Client{AppID: "a", ClientSecret: "b", tm: newTokenManager("a", "b")}
	_, err := c.request(context.Background(), "tok-1", http.MethodPost, srv.URL,
		&messagePayload{Content: "hi", MsgType: 0})
	ae, ok := err.(*apiErr)
	if !ok {
		t.Fatalf("expect *apiErr, got %T: %v", err, err)
	}
	if ae.Status != 400 || ae.Code != 123 || ae.Message != "bad request" {
		t.Fatalf("apiErr mismatch: %+v", ae)
	}
	if IsAuthErr(err) {
		t.Fatalf("400 should not be auth error")
	}
}

// TestRequestNotFound 确认 401 判定为鉴权错误
func TestRequestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"token expired"}`)
	}))
	defer srv.Close()

	c := &Client{tm: newTokenManager("a", "b")}
	_, err := c.request(context.Background(), "tok", http.MethodPost, srv.URL, nil)
	if err == nil || !IsAuthErr(err) {
		t.Fatalf("expect auth error, got %v", err)
	}
}

package qqbot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// API 基址
const (
	apiBaseURL       = "https://api.sgroup.qq.com"
	apiSandboxURL    = "https://sandbox.api.sgroup.qq.com"
	authHeaderPrefix = "QQBot "
)

// ChatTarget 消息目标：C2C 私聊或群聊
type ChatTarget struct {
	Kind   string // "c2c" / "group"
	OpenID string
}

// Client QQ 开放平台机器人 REST 客户端
type Client struct {
	AppID        string
	ClientSecret string
	Sandbox      bool // 沙箱环境（未上线的机器人测试用）

	tm *tokenManager
}

// NewClient 创建客户端
func NewClient(appID, clientSecret string, sandbox bool) *Client {
	return &Client{
		AppID:        appID,
		ClientSecret: clientSecret,
		Sandbox:      sandbox,
		tm:           newTokenManager(appID, clientSecret),
	}
}

// GetToken 获取并缓存 access_token（Start 时用于校验凭据有效性）
func (c *Client) GetToken(ctx context.Context) (string, error) {
	return c.tm.Token(ctx)
}

func (c *Client) baseURL() string {
	if c.Sandbox {
		return apiSandboxURL
	}
	return apiBaseURL
}

// apiErr API 错误（含 HTTP 状态码，供调用方判断是否 token 失效）
type apiErr struct {
	Status  int
	Code    int
	Message string
}

func (e *apiErr) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("qqbot api: HTTP %d code=%d: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("qqbot api: HTTP %d: %s", e.Status, e.Message)
}

// IsAuthErr 判断是否鉴权类错误（token 过期等，可刷新重试）
func IsAuthErr(err error) bool {
	ae, ok := err.(*apiErr)
	return ok && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// maxUploadBytes QQ 富媒体上传的原始文件大小预检上限。
// 官方文档未标注数值；实测（2026-10 二分探测）STGW 网关对请求体上限为 20MB
// （21MB 请求体返回 413），file_data 需 base64 编码（体积 ×4/3），
// 故原始文件上限 ≈ 20MB×3/4 = 15MB。参考：官方 openclaw-qqbot 插件自述图片 20MB。
const maxUploadBytes = 15_000_000

// IsPayloadTooLarge 判断是否超过 QQ 富媒体上传大小限制（HTTP 413）
func IsPayloadTooLarge(err error) bool {
	ae, ok := err.(*apiErr)
	return ok && ae.Status == http.StatusRequestEntityTooLarge
}

// do 带鉴权的 JSON API 调用（token 失效自动刷新重试一次）
func (c *Client) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	tok, err := c.tm.Token(ctx)
	if err != nil {
		return nil, err
	}
	data, err := c.request(ctx, tok, method, c.baseURL()+path, body)
	if IsAuthErr(err) { // token 可能刚过期：强制刷新重试
		c.tm.invalidate()
		if tok2, terr := c.tm.Token(ctx); terr == nil {
			return c.request(ctx, tok2, method, c.baseURL()+path, body)
		}
	}
	return data, err
}

func (c *Client) request(ctx context.Context, token, method, fullURL string, body interface{}) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authHeaderPrefix+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiErr{Status: resp.StatusCode, Message: string(data)}
		var er struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &er) == nil && er.Message != "" {
			ae.Code, ae.Message = er.Code, er.Message
		}
		return nil, ae
	}
	return data, nil
}

// messagePayload 发消息请求体（msg_id 非空为被动回复）
type messagePayload struct {
	Content string `json:"content,omitempty"`
	MsgType int    `json:"msg_type"`
	MsgID   string `json:"msg_id,omitempty"`
	MsgSeq  int    `json:"msg_seq,omitempty"`
	Media   *media `json:"media,omitempty"`
}

type media struct {
	FileInfo string `json:"file_info"`
}

// messagesPath 按 target 生成消息/文件端点
func messagesPath(chat ChatTarget, suffix string) string {
	if chat.Kind == "group" {
		return "/v2/groups/" + chat.OpenID + "/" + suffix
	}
	return "/v2/users/" + chat.OpenID + "/" + suffix
}

// SendText 发送文本消息（msgID 非空为被动回复；seq 用于同一 msg_id 的多条回复去重）
func (c *Client) SendText(ctx context.Context, chat ChatTarget, text, msgID string, seq int) error {
	p := messagePayload{Content: text, MsgType: 0, MsgID: msgID, MsgSeq: seq}
	_, err := c.do(ctx, http.MethodPost, messagesPath(chat, "messages"), &p)
	return err
}

// SendFile 发送本地文件（音频走文件消息，规避语音 silk 格式要求）。
// 先上传到 files 端点（srv_send_msg=false），再以 msg_type=7 富媒体消息发出。
func (c *Client) SendFile(ctx context.Context, chat ChatTarget, path, caption, msgID string, seq int) error {
	fileType := 4 // 文件
	switch ext := filepath.Ext(path); ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		fileType = 1
	case ".mp4", ".mov":
		fileType = 2
	case ".silk", ".amr":
		fileType = 3
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("qqbot: open file: %w", err)
	}
	defer f.Close()

	// 上传前大小预检：超过网关限制直接返回 413 语义错误，避免白传一坨数据
	if fi, serr := f.Stat(); serr == nil && fi.Size() > maxUploadBytes {
		return &apiErr{
			Status:  http.StatusRequestEntityTooLarge,
			Message: fmt.Sprintf("file too large: %d bytes > %d (QQ 富媒体单文件上限约 15MB)", fi.Size(), maxUploadBytes),
		}
	}

	// 实测（2026-10）：files 端点必须是 JSON 请求体，file_data 为 base64 字符串
	// （multipart 文件流会报 40093006/40093007，multipart base64 字段服务端不解码，
	// 收到的文件是 5.21MB 的编码文本）。文件名经 filename/file_name 字段下发。
	raw, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("qqbot: read file: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	var fr struct {
		FileUUID  string `json:"file_uuid"`
		FileInfo  string `json:"file_info"`
		FilePerms string `json:"file_perms"`
	}
	// 上传偶发 850012「call inner proxy error」瞬时错误，重试一次
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		lastErr = c.uploadMedia(ctx, chat, fileType, b64, filepath.Base(path), &fr)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return lastErr
	}
	if fr.FileInfo == "" {
		return fmt.Errorf("qqbot: upload file got empty file_info")
	}

	// 富媒体消息发出（content 作为附带文本说明）
	p := messagePayload{Content: caption, MsgType: 7, MsgID: msgID, MsgSeq: seq, Media: &media{FileInfo: fr.FileInfo}}
	_, err = c.do(ctx, http.MethodPost, messagesPath(chat, "messages"), &p)
	return err
}

// uploadMedia 上传文件到 files 端点（仅获取 file_info，不直接发送消息）。
// 请求体为 JSON：file_type（媒体类型）、file_data（base64 字符串）、
// filename/file_name（文件名，官方文档未记载但实测生效，双写兼容）、srv_send_msg=false。
func (c *Client) uploadMedia(ctx context.Context, chat ChatTarget, fileType int, b64, filename string, out *struct {
	FileUUID  string `json:"file_uuid"`
	FileInfo  string `json:"file_info"`
	FilePerms string `json:"file_perms"`
}) error {
	payload := map[string]interface{}{
		"file_type":    fileType,
		"file_data":    b64,
		"srv_send_msg": false,
	}
	if filename != "" {
		payload["filename"] = filename
		payload["file_name"] = filename
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	tok, err := c.tm.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+messagesPath(chat, "files"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeaderPrefix+tok)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute} // 音频文件可能较大
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiErr{Status: resp.StatusCode, Message: string(respData)}
		var er struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respData, &er) == nil && er.Message != "" {
			ae.Code, ae.Message = er.Code, er.Message
		}
		return ae
	}
	return json.Unmarshal(respData, out)
}

package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"nas-agent/internal/app/common/util"
)

// Gateway op 码（QQ 机器人官方定义）
const (
	opDispatch        = 0 // 服务端推送事件
	opHeartbeat       = 1 // 客户端心跳（带最新 s）
	opIdentify        = 2 // 鉴权连接
	opResume          = 6 // 断线恢复
	opReconnect       = 7 // 服务端要求重连
	opInvalidSession  = 9 // 无效会话
	opHello           = 10 // 连接建立（含 heartbeat_interval）
	opHeartbeatACK    = 11 // 心跳确认
)

// intentC2CGroupAtEvents C2C 私聊 + 群聊 @ 消息事件
const intentC2CGroupAtEvents = 1 << 25

// gatewayURL 生产环境网关地址
const gatewayURL = "wss://api.sgroup.qq.com/websocket/"

// Event 网关分发事件（仅业务关心字段）
type Event struct {
	Type    string // t 字段：C2C_MESSAGE_CREATE / GROUP_AT_MESSAGE_CREATE / READY 等
	MsgID   string // 消息 ID（被动回复凭据，5 分钟有效）
	Content string // 文本内容
	Chat    ChatTarget
	Sender  string // 发送者 openid
}

// gatewayPayload 网关报文外层
type gatewayPayload struct {
	Op int             `json:"op"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	D  json.RawMessage `json:"d"`
}

// helloPayload op10 Hello
type helloPayload struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

// identifyPayload op2 Identify
type identifyPayload struct {
	Token   string `json:"token"`
	Intents int    `json:"intents"`
	Shard   [2]int `json:"shard"`
}

// readyPayload t=READY 事件（恢复会话所需）
type readyPayload struct {
	SessionID string `json:"session_id"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

// c2cMessagePayload t=C2C_MESSAGE_CREATE
type c2cMessagePayload struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Attachments []struct {
		ContentType string `json:"content_type"`
	} `json:"attachments"`
	Author struct {
		UserOpenid string `json:"user_openid"`
	} `json:"author"`
}

// groupMessagePayload t=GROUP_AT_MESSAGE_CREATE
type groupMessagePayload struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	GroupOpenid string `json:"group_openid"`
	Author      struct {
		MemberOpenid string `json:"member_openid"`
	} `json:"author"`
}

// Run 阻塞运行网关：连接、鉴权、心跳、事件分发、断线自动重连（指数退避 + Resume）。
// ctx 取消后优雅退出。onEvent 在网关 goroutine 中被顺序调用。
func (c *Client) Run(ctx context.Context, onEvent func(Event)) error {
	sessionID := ""
	lastSeq := int64(0)
	backoff := time.Second

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.runOnce(ctx, &sessionID, &lastSeq, onEvent)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			util.Logger.Errorf("qqbot: gateway error: %v", err)
		}
		// 无效会话时丢弃 session，退避后全新连接
		if sessionID == "" {
			backoff *= 2
		} else {
			backoff = time.Second
		}
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
		util.Logger.Infof("qqbot: reconnecting in %s (session=%q)", backoff, sessionID)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// runOnce 单次连接生命周期：Hello → Identify/Resume → 心跳与 Dispatch 循环
func (c *Client) runOnce(ctx context.Context, sessionID *string, lastSeq *int64, onEvent func(Event)) error {
	conn, err := wsDial(ctx, gatewayURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	// op10 Hello
	_, data, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var hello gatewayPayload
	if err := json.Unmarshal(data, &hello); err != nil || hello.Op != opHello {
		return fmt.Errorf("unexpected first frame: op=%d err=%v", hello.Op, err)
	}
	interval := hello.D
	var hb helloPayload
	_ = json.Unmarshal(interval, &hb)
	if hb.HeartbeatInterval <= 0 {
		hb.HeartbeatInterval = 30000
	}

	// Identify（无会话）或 Resume（有会话）
	if *sessionID == "" {
		tok, err := c.tm.Token(ctx)
		if err != nil {
			return err
		}
		idf := gatewayPayload{Op: opIdentify}
		idf.D, _ = json.Marshal(identifyPayload{
			Token:   "QQBot " + tok,
			Intents: intentC2CGroupAtEvents,
			Shard:   [2]int{0, 1},
		})
		if data, err = json.Marshal(idf); err != nil {
			return err
		}
	} else {
		tok, terr := c.tm.Token(ctx)
		if terr != nil {
			return terr
		}
		rs := gatewayPayload{Op: opResume}
		rs.D, _ = json.Marshal(map[string]interface{}{
			"token":      "QQBot " + tok,
			"session_id": *sessionID,
			"seq":        atomic.LoadInt64(lastSeq),
		})
		if data, err = json.Marshal(rs); err != nil {
			return err
		}
	}
	if err := conn.WriteText(data); err != nil {
		return fmt.Errorf("send identify/resume: %w", err)
	}

	// 心跳协程
	var hbErr error
	var hbOnce sync.Once
	hbStop := make(chan struct{})
	go func() {
		defer close(hbStop)
		tick := time.NewTicker(time.Duration(hb.HeartbeatInterval) * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				hbMsg := gatewayPayload{Op: opHeartbeat, S: atomic.LoadInt64(lastSeq)}
				b, _ := json.Marshal(hbMsg)
				if err := conn.WriteText(b); err != nil {
					hbOnce.Do(func() { hbErr = err })
					return
				}
			case <-hbStop:
				return
			}
		}
	}()
	defer func() { <-hbStop }() // 等心跳协程退出（连接关闭后其写入必然失败返回）

	// 会话期间 token 定期刷新由 tokenManager 惰性处理，无需额外协程
	invalidSession := false
	readErr := error(nil)
	for {
		_, data, err = conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		var p gatewayPayload
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if p.S > 0 {
			atomic.StoreInt64(lastSeq, p.S)
		}
		switch p.Op {
		case opDispatch:
			c.handleDispatch(p, sessionID, onEvent)
		case opHeartbeatACK:
			// 心跳正常
		case opReconnect:
			util.Logger.Infof("qqbot: server requested reconnect")
			return nil // 保留 session，外层 Resume
		case opInvalidSession:
			util.Logger.Errorf("qqbot: invalid session, start fresh")
			*sessionID = ""
			invalidSession = true
		default:
			// op10 等其他帧忽略
		}
		if invalidSession {
			// 收到 op9 后服务端可能直接关闭连接，主动退出走重连
			return fmt.Errorf("invalid session")
		}
	}
	if hbErr != nil {
		return fmt.Errorf("heartbeat: %w", hbErr)
	}
	return readErr
}

// handleDispatch 处理 op0 事件
func (c *Client) handleDispatch(p gatewayPayload, sessionID *string, onEvent func(Event)) {
	switch p.T {
	case "READY":
		var rd readyPayload
		if err := json.Unmarshal(p.D, &rd); err == nil {
			*sessionID = rd.SessionID
			util.Logger.Infof("qqbot: gateway ready, bot=%s(%s) session=%s", rd.User.Username, rd.User.ID, rd.SessionID)
		}
	case "RESUMED":
		util.Logger.Infof("qqbot: gateway resumed")
	case "C2C_MESSAGE_CREATE":
		var m c2cMessagePayload
		if err := json.Unmarshal(p.D, &m); err != nil {
			return
		}
		onEvent(Event{
			Type:    p.T,
			MsgID:   m.ID,
			Content: m.Content,
			Chat:    ChatTarget{Kind: "c2c", OpenID: m.Author.UserOpenid},
			Sender:  m.Author.UserOpenid,
		})
	case "GROUP_AT_MESSAGE_CREATE":
		var m groupMessagePayload
		if err := json.Unmarshal(p.D, &m); err != nil {
			return
		}
		onEvent(Event{
			Type:    p.T,
			MsgID:   m.ID,
			Content: m.Content,
			Chat:    ChatTarget{Kind: "group", OpenID: m.GroupOpenid},
			Sender:  m.Author.MemberOpenid,
		})
	default:
		// 频道等其他事件暂不处理
	}
}

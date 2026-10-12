// Package qqbot 实现 QQ 开放平台官方机器人协议（WebSocket 网关 + REST API），
// 纯 Go 标准库实现，不依赖第三方 WebSocket 库。
package qqbot

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// wsConn RFC 6455 WebSocket 客户端连接（仅实现协议必需子集）
type wsConn struct {
	conn   net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex // 写锁（心跳与发送可能并发）
	closed bool
}

// WebSocket 帧操作码
const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// errWSClosed 连接已关闭
var errWSClosed = errors.New("websocket: connection closed")

// wsDial 建立 WebSocket 连接（手工完成 HTTP Upgrade 握手）
func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket: parse url: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("websocket: dial %s: %w", host, err)
	}
	if u.Scheme == "wss" {
		h, _, _ := net.SplitHostPort(host)
		nc = tls.Client(nc, &tls.Config{ServerName: h})
	}

	// 客户端握手：Sec-WebSocket-Key 随机 16 字节 base64
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := nc.Write([]byte(req)); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: write handshake: %w", err)
	}

	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: read handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: handshake HTTP %d: %s", resp.StatusCode, body)
	}
	// 校验 Sec-WebSocket-Accept = base64(sha1(key + 固定 GUID))
	want := base64.StdEncoding.EncodeToString(sha1Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")))
	if resp.Header.Get("Sec-WebSocket-Accept") != want {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket: Sec-WebSocket-Accept mismatch")
	}
	return &wsConn{conn: nc, br: br}, nil
}

// ReadMessage 读取一条完整消息（自动回 Pong、处理 Close）。返回 op 与 payload。
func (c *wsConn) ReadMessage() (int, []byte, error) {
	for {
		op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing: // 自动回 pong
			_ = c.writeFrame(wsOpPong, data)
		case wsOpPong: // 忽略
		case wsOpClose:
			_ = c.writeFrame(wsOpClose, data)
			_ = c.Close()
			return 0, nil, errWSClosed
		case wsOpText, wsOpBinary:
			return int(op), data, nil
		case wsOpContinuation:
			// 非分片协议（QQ 网关不会用），丢弃避免死循环
		default:
			return 0, nil, fmt.Errorf("websocket: unexpected op %d", op)
		}
	}
}

// WriteText 发送一条文本帧（加锁避免与心跳并发交错）
func (c *wsConn) WriteText(data []byte) error {
	return c.writeFrame(wsOpText, data)
}

func (c *wsConn) readFrame() (op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 16<<20 { // 防御：单帧上限 16MB
		return 0, nil, fmt.Errorf("websocket: frame too large: %d", length)
	}
	// RFC 6455：MASK=1 时 Masking-key 位于 Payload 之前
	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked { // 服务端帧按规范不掩码，兼容处理
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return op, payload, nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errWSClosed
	}
	var hdr [14]byte
	hdr[0] = 0x80 | op // FIN=1
	n := len(payload)
	maskBit := byte(0x80)
	pos := 2
	switch {
	case n < 126:
		hdr[1] = maskBit | byte(n)
	case n < 1<<16:
		hdr[1] = maskBit | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		pos = 4
	default:
		hdr[1] = maskBit | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		pos = 10
	}
	// 客户端帧必须掩码
	var maskKey [4]byte
	_, _ = rand.Read(maskKey[:])
	copy(hdr[pos:pos+4], maskKey[:])
	pos += 4

	buf := make([]byte, 0, pos+n)
	buf = append(buf, hdr[:pos]...)
	for i := 0; i < n; i++ { // 就地掩码写入
		buf = append(buf, payload[i]^maskKey[i%4])
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.conn.Write(buf)
	return err
}

// Close 关闭底层连接
func (c *wsConn) Close() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

func sha1Sum(b []byte) []byte {
	s := sha1.New()
	s.Write(b)
	return s.Sum(nil)
}

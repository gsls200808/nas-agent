package qqbot

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// wsTestServer 启动一个最小 WebSocket 测试服务器，完成握手后交给 handle 处理
func wsTestServer(t *testing.T, handle func(conn net.Conn, br *bufio.Reader)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			return
		}
		accept := base64.StdEncoding.EncodeToString(sha1Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")))
		resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		handle(conn, br)
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// srvReadFrame 服务端读一帧（客户端帧带掩码）
func srvReadFrame(t *testing.T, br *bufio.Reader) (op byte, payload []byte) {
	t.Helper()
	var hdr [2]byte
	if _, err := readFull(br, hdr[:]); err != nil {
		t.Fatalf("server read header: %v", err)
	}
	op = hdr[0] & 0x0F
	n := int(hdr[1] & 0x7F)
	ext := make([]byte, map[uint64]int{126: 2, 127: 8}[uint64(n)])
	if len(ext) > 0 {
		if _, err := readFull(br, ext); err != nil {
			t.Fatalf("server read ext len: %v", err)
		}
		if len(ext) == 2 {
			n = int(binary.BigEndian.Uint16(ext))
		} else {
			n = int(binary.BigEndian.Uint64(ext))
		}
	}
	masked := hdr[1]&0x80 != 0
	// RFC 6455：Masking-key 在 Payload 之前
	var mk [4]byte
	if masked {
		if _, err := readFull(br, mk[:]); err != nil {
			t.Fatalf("server read mask: %v", err)
		}
	}
	payload = make([]byte, n)
	if _, err := readFull(br, payload); err != nil {
		t.Fatalf("server read payload: %v", err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	return op, payload
}

// srvWriteFrame 服务端发一帧（服务端帧不掩码）
func srvWriteFrame(t *testing.T, conn net.Conn, op byte, payload []byte) {
	t.Helper()
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 1<<16:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
	}
	if _, err := conn.Write(append(hdr, payload...)); err != nil {
		t.Fatalf("server write frame: %v", err)
	}
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	return io.ReadFull(br, buf)
}

func TestWSDialEcho(t *testing.T) {
	msg := []byte("你好, websocket!")
	ln := wsTestServer(t, func(conn net.Conn, br *bufio.Reader) {
		op, payload := srvReadFrame(t, br)
		if op != wsOpText {
			t.Errorf("server: expect text op, got %d", op)
		}
		srvWriteFrame(t, conn, wsOpText, payload)
	})

	c, err := wsDial(testCtx(), "ws://"+ln.Addr().String()+"/echo")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.WriteText(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != int(wsOpText) || string(data) != string(msg) {
		t.Fatalf("echo mismatch: op=%d data=%q", op, data)
	}
}

func TestWSAutoPong(t *testing.T) {
	ln := wsTestServer(t, func(conn net.Conn, br *bufio.Reader) {
		srvWriteFrame(t, conn, wsOpPing, []byte("hb"))
		op, _ := srvReadFrame(t, br)
		if op != wsOpPong {
			t.Errorf("server: expect pong, got %d", op)
		}
	})
	c, err := wsDial(testCtx(), "ws://"+ln.Addr().String()+"/ping")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// 客户端应自动回 pong，且不把 ping 当作数据帧返回；随后服务端关闭 → ReadMessage 报关闭
	go func() {
		time.Sleep(200 * time.Millisecond)
		srvWriteFrame(t, c.conn, wsOpClose, nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	c.conn.SetReadDeadline(deadline)
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatalf("expect close error, got nil")
	}
}

func TestWSLargeFrame(t *testing.T) {
	// 300 字节 → 16 位扩展长度编码
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	ln := wsTestServer(t, func(conn net.Conn, br *bufio.Reader) {
		_, data := srvReadFrame(t, br)
		if string(data) != string(payload) {
			t.Errorf("server: payload mismatch len=%d", len(data))
		}
		srvWriteFrame(t, conn, wsOpBinary, data)
	})
	c, err := wsDial(testCtx(), "ws://"+ln.Addr().String()+"/big")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.writeFrame(wsOpBinary, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != int(wsOpBinary) || string(data) != string(payload) {
		t.Fatalf("large frame mismatch: op=%d len=%d", op, len(data))
	}
}

func TestWSHandshakeAcceptMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		// 返回错误的 Accept
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: badauth\r\n\r\n"))
	}()
	if _, err := wsDial(testCtx(), "ws://"+ln.Addr().String()+"/bad"); err == nil {
		t.Fatalf("expect handshake error, got nil")
	}
}

// testCtx 测试用 context
func testCtx() context.Context { return context.Background() }

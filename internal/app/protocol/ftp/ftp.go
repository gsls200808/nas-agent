package ftp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/spec"
)

var _ spec.Client = (*Client)(nil)

// Client 纯标准库实现的 FTP 客户端（RFC 959，被动模式）
type Client struct {
	cfg  spec.Config
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
}

// New 创建 FTP 客户端
func New(cfg spec.Config) *Client {
	return &Client{cfg: cfg}
}

// ftpReply 读取一条 FTP 应答，支持多行（"220-..."）
func (c *Client) reply() (int, string, error) {
	var code int
	var lines []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			continue
		}
		n, err := strconv.Atoi(line[:3])
		if err != nil {
			continue
		}
		code = n
		lines = append(lines, line)
		if len(line) >= 4 && line[3] == ' ' {
			break
		}
	}
	return code, strings.Join(lines, "\n"), nil
}

func (c *Client) cmd(format string, args ...interface{}) (int, string, error) {
	if _, err := fmt.Fprintf(c.bw, format+"\r\n", args...); err != nil {
		return 0, "", err
	}
	if err := c.bw.Flush(); err != nil {
		return 0, "", err
	}
	return c.reply()
}

// expect 校验应答码前缀（如 2 表示 2xx）
func (c *Client) expect(prefix int, format string, args ...interface{}) (string, error) {
	code, msg, err := c.cmd(format, args...)
	if err != nil {
		return "", err
	}
	if code/100 != prefix {
		return "", fmt.Errorf("ftp: %s -> %d %s", strings.TrimSuffix(format, " "), code, firstLine(msg))
	}
	return msg, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Open 连接并登录
func (c *Client) Open() error {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("ftp dial: %w", err)
	}
	c.conn = conn
	c.br = bufio.NewReader(conn)
	c.bw = bufio.NewWriter(conn)
	if _, _, err := c.reply(); err != nil {
		return fmt.Errorf("ftp greeting: %w", err)
	}
	user := c.cfg.Username
	if user == "" {
		user = "anonymous"
	}
	if _, err := c.expect(3, "USER %s", user); err != nil {
		return err
	}
	if _, err := c.expect(2, "PASS %s", c.cfg.Password); err != nil {
		return err
	}
	_, _, _ = c.cmd("TYPE I")
	_, _, _ = c.cmd("FEAT")
	// 进入起始目录
	if base := c.baseDir(); base != "" {
		if _, err := c.expect(2, "CWD %s", base); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) baseDir() string {
	b := strings.Trim(c.cfg.Share, "/")
	return b
}

// fullPath 虚拟路径（相对 share）转 FTP 绝对路径
func (c *Client) fullPath(p string) string {
	p = path.Clean("/" + p)
	base := c.baseDir()
	if base == "" {
		return p
	}
	if p == "/" {
		return "/" + base
	}
	return "/" + base + p
}

// Close 退出并断开
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	_, _, _ = c.cmd("QUIT")
	err := c.conn.Close()
	c.conn = nil
	return err
}

// pasv 进入被动模式并返回数据连接
func (c *Client) pasv() (net.Conn, error) {
	msg, err := c.expect(2, "PASV")
	if err != nil {
		return nil, err
	}
	start := strings.Index(msg, "(")
	end := strings.LastIndex(msg, ")")
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("ftp: bad PASV reply: %s", msg)
	}
	parts := strings.Split(msg[start+1:end], ",")
	if len(parts) != 6 {
		return nil, fmt.Errorf("ftp: bad PASV address: %s", msg)
	}
	nums := make([]int, 6)
	for i := 0; i < 6; i++ {
		nums[i], _ = strconv.Atoi(strings.TrimSpace(parts[i]))
	}
	host := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])
	port := nums[4]*256 + nums[5]
	dataConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("ftp: dial data conn: %w", err)
	}
	return dataConn, nil
}

// Stat 实现在 parse.go（MLST + LIST 回退）

// List 列目录
func (c *Client) List(dir string) ([]model.FileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, fmt.Errorf("ftp: not connected")
	}
	// 优先尝试 MLSD
	if entries, err := c.mlsd(dir); err == nil {
		return entries, nil
	}
	return c.list(dir)
}

// mlsd 使用 MLSD 命令（机器可解析格式）
func (c *Client) mlsd(dir string) ([]model.FileInfo, error) {
	dataConn, err := c.pasv()
	if err != nil {
		return nil, err
	}
	code, msg, err := c.cmd("MLSD %s", c.fullPath(dir))
	if err != nil || code/100 != 1 {
		dataConn.Close()
		return nil, fmt.Errorf("mlsd unavailable: %d %s %v", code, msg, err)
	}
	data, err := io.ReadAll(dataConn)
	_ = dataConn.Close()
	if err != nil {
		return nil, err
	}
	if _, _, err := c.reply(); err != nil {
		return nil, err
	}
	var out []model.FileInfo
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		if sp < 0 {
			continue
		}
		facts := line[:sp]
		name := line[sp+1:]
		fi := model.FileInfo{Name: path.Base(name), Path: path.Join(path.Clean("/"+dir), name)}
		isDir := false
		for _, kv := range strings.Split(facts, ";") {
			eq := strings.Index(kv, "=")
			if eq < 0 {
				continue
			}
			k := strings.ToLower(kv[:eq])
			v := kv[eq+1:]
			switch k {
			case "type":
				isDir = strings.EqualFold(v, "dir") || strings.EqualFold(v, "cdir") || strings.EqualFold(v, "pdir")
			case "size":
				fi.Size, _ = strconv.ParseInt(v, 10, 64)
			case "modify":
				if t, err := time.Parse("20060102150405", v); err == nil {
					fi.ModTime = t
				}
			}
		}
		fi.IsDir = isDir
		// 跳过 . / ..
		if name == "." || name == ".." {
			continue
		}
		out = append(out, fi)
	}
	return out, nil
}

// list 传统 LIST 解析（UNIX 与 DOS 风格）
func (c *Client) list(dir string) ([]model.FileInfo, error) {
	dataConn, err := c.pasv()
	if err != nil {
		return nil, err
	}
	code, msg, err := c.cmd("LIST %s", c.fullPath(dir))
	if err != nil || code/100 != 1 {
		dataConn.Close()
		return nil, fmt.Errorf("ftp list: %d %s %v", code, msg, err)
	}
	data, err := io.ReadAll(dataConn)
	_ = dataConn.Close()
	if err != nil {
		return nil, err
	}
	if _, _, err := c.reply(); err != nil {
		return nil, err
	}
	return parseLIST(string(data), dir), nil
}

// Download 下载，返回数据流
func (c *Client) Download(p string) (io.ReadCloser, error) {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("ftp: not connected")
	}
	dataConn, err := c.pasv()
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	code, msg, err := c.cmd("RETR %s", c.fullPath(p))
	if err != nil || code/100 != 1 {
		dataConn.Close()
		c.mu.Unlock()
		return nil, fmt.Errorf("ftp retr: %d %s %v", code, msg, err)
	}
	return &ftpReader{Conn: dataConn, c: c}, nil
}

// ftpReader 数据连接读取结束后读取控制连接 226 应答并释放锁
type ftpReader struct {
	net.Conn
	c      *Client
	closed bool
}

func (r *ftpReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.Conn.Close()
	_, _, _ = r.c.reply() // 226 Transfer complete
	r.c.mu.Unlock()
	return err
}

// Upload 上传
func (c *Client) Upload(p string, r io.Reader, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("ftp: not connected")
	}
	dataConn, err := c.pasv()
	if err != nil {
		return err
	}
	code, msg, err := c.cmd("STOR %s", c.fullPath(p))
	if err != nil || code/100 != 1 {
		dataConn.Close()
		return fmt.Errorf("ftp stor: %d %s %v", code, msg, err)
	}
	if _, err := io.Copy(dataConn, r); err != nil {
		dataConn.Close()
		return err
	}
	_ = dataConn.Close()
	if _, _, err := c.reply(); err != nil {
		return err
	}
	return nil
}

// Mkdir 创建目录
func (c *Client) Mkdir(p string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.expect(2, "MKD %s", c.fullPath(p))
	return err
}

// Remove 删除文件或目录
func (c *Client) Remove(p string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 先尝试 DELE，失败再尝试 RMD
	if code, _, err := c.cmd("DELE %s", c.fullPath(p)); err == nil && code/100 == 2 {
		return nil
	}
	_, err := c.expect(2, "RMD %s", c.fullPath(p))
	return err
}

// Rename 重命名
func (c *Client) Rename(from, to string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.expect(3, "RNFR %s", c.fullPath(from)); err != nil {
		return err
	}
	_, err := c.expect(2, "RNTO %s", c.fullPath(to))
	return err
}

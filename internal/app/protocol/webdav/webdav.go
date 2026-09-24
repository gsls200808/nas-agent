package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/spec"
)

var _ spec.Client = (*Client)(nil)

// Client 纯标准库 net/http 实现的 WebDAV 客户端（RFC 4918）
type Client struct {
	cfg     spec.Config
	baseURL string // http://host:port
	httpCli *http.Client
}

const (
	// metaTimeout 元数据类操作（PROPFIND/MKCOL/DELETE/MOVE）的单次请求超时
	metaTimeout = 30 * time.Second
	// uploadTimeout 上传的总超时，仅作为连接挂死时的兜底；大文件上传本身可能耗时很久
	uploadTimeout = 6 * time.Hour
)

// New 创建 WebDAV 客户端
//
// 不设置 http.Client.Timeout（整条请求的总时长上限），也不设置 Transport.ResponseHeaderTimeout
// （请求体发送完毕后等待响应头的上限）：网盘型 WebDAV（如 openlist 代理 SMB）会先把整个文件落地
// 到后端存储，之后才返回响应头，这两个上限都会把大文件上传拦腰截断。
// 改为按请求设置超时：元数据操作用短超时，上传用很长的兜底超时，读取下载流不设超时。
func New(cfg spec.Config) *Client {
	return &Client{
		cfg:     cfg,
		baseURL: fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
		httpCli: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:    10,
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}
}

// ---------- PROPFIND XML（命名空间无关解析） ----------

type msResponse struct {
	Href     string       `xml:"href"`
	Propstat []msPropstat `xml:"propstat"`
}

type msPropstat struct {
	Status string `xml:"status"`
	Prop   msProp `xml:"prop"`
}

type msProp struct {
	DisplayName   string         `xml:"displayname"`
	ContentLength int64          `xml:"getcontentlength"`
	LastModified  string         `xml:"getlastmodified"`
	ResourceType  msResourceType `xml:"resourcetype"`
}

type msResourceType struct {
	Collection *struct{} `xml:"collection"`
}

type msMultistatus struct {
	Responses []msResponse `xml:"response"`
}

// Open 测试连通性
func (c *Client) Open() error {
	req, err := http.NewRequest("OPTIONS", c.baseURL+"/", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("webdav: authentication failed")
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("webdav: server status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Close() error { return nil }

func (c *Client) setAuth(req *http.Request) {
	if c.cfg.Username != "" || c.cfg.Password != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
}

// shareBase 规范化 share 作为基础路径（以 / 开头）
func (c *Client) shareBase() string {
	b := c.cfg.Share
	if b == "" {
		return ""
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return strings.TrimSuffix(b, "/")
}

// fullURL 将虚拟路径（相对 share）拼接为完整 URL，逐段转义
func (c *Client) fullURL(p string) string {
	p = path.Clean("/" + p)
	if p == "/" {
		return c.baseURL + c.shareBase() + "/"
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.baseURL + c.shareBase() + "/" + strings.Join(segs, "/")
}

// hrefToPath 把 PROPFIND 返回的 href 转为相对 share 的虚拟路径
func (c *Client) hrefToPath(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	p := u.Path
	base := c.shareBase()
	if base != "" {
		if strings.HasPrefix(p, base+"/") {
			p = strings.TrimPrefix(p, base)
		} else if p == base {
			p = "/"
		}
	}
	if p == "" {
		p = "/"
	}
	return p
}

// do 发起请求；ctx 为 nil 表示不限制本次请求时长；contentLength >= 0 时显式设置请求体长度
// （否则未知长度的 body 会退化为 chunked 传输编码）
func (c *Client) do(ctx context.Context, method, urlPath string, body io.Reader, contentLength int64, extraHeaders map[string]string) (*http.Response, error) {
	var (
		req *http.Request
		err error
	)
	if ctx != nil {
		req, err = http.NewRequestWithContext(ctx, method, urlPath, body)
	} else {
		req, err = http.NewRequest(method, urlPath, body)
	}
	if err != nil {
		return nil, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	c.setAuth(req)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpCli.Do(req)
}

// metaCtx 元数据操作使用的短超时上下文
func metaCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), metaTimeout)
}

// Stat 查询单个条目
func (c *Client) Stat(p string) (*model.FileInfo, error) {
	list, err := c.propfind(p, 0)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("webdav: %s not found", p)
	}
	return &list[0], nil
}

// List 列目录
func (c *Client) List(dir string) ([]model.FileInfo, error) {
	list, err := c.propfind(dir, 1)
	if err != nil {
		return nil, err
	}
	// 去掉目录自身
	result := make([]model.FileInfo, 0, len(list))
	for _, fi := range list {
		if fi.Path == path.Clean("/"+dir) && fi.IsDir {
			continue
		}
		result = append(result, fi)
	}
	return result, nil
}

func (c *Client) propfind(p string, depth int) ([]model.FileInfo, error) {
	ctx, cancel := metaCtx()
	defer cancel()
	resp, err := c.do(ctx, "PROPFIND", c.fullURL(p), nil, -1, map[string]string{
		"Depth":        strconv.Itoa(depth),
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("webdav: %s not found", p)
	}
	if resp.StatusCode != http.StatusMultiStatus {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("webdav: PROPFIND status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ms msMultistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("webdav: parse propfind: %w", err)
	}
	out := make([]model.FileInfo, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		var prop msProp
		for _, ps := range r.Propstat {
			if strings.Contains(ps.Status, "200") {
				prop = ps.Prop
				break
			}
			prop = ps.Prop
		}
		vp := c.hrefToPath(r.Href)
		isDir := prop.ResourceType.Collection != nil
		if !isDir && strings.HasSuffix(vp, "/") {
			isDir = true
		}
		// 归一化路径（PROPFIND 对集合返回的 href 带尾斜杠，与其它协议保持一致）
		vp = path.Clean(vp)
		name := path.Base(vp)
		if vp == "/" {
			name = "/"
		}
		fi := model.FileInfo{
			Name:  name,
			Path:  vp,
			IsDir: isDir,
			Size:  prop.ContentLength,
		}
		if prop.LastModified != "" {
			if t, err := http.ParseTime(prop.LastModified); err == nil {
				fi.ModTime = t
			}
		}
		out = append(out, fi)
	}
	return out, nil
}

// Download 下载
func (c *Client) Download(p string) (io.ReadCloser, error) {
	// 不限制时长：返回的流由调用方持续读取，无法在拿到响应头后取消上下文
	resp, err := c.do(nil, "GET", c.fullURL(p), nil, -1, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("webdav: GET status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// Upload 上传
//
// size >= 0 时显式设置请求体长度，避免退化为 chunked 传输编码（openlist 等网盘型
// WebDAV 对 chunked 上传的处理较差）。
func (c *Client) Upload(p string, r io.Reader, size int64) error {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	// 网盘型 WebDAV 会先落地整个文件再返回响应头，这里只设很长的兜底超时
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	resp, err := c.do(ctx, "PUT", c.fullURL(p), r, size, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webdav: PUT status %d", resp.StatusCode)
	}
	return nil
}

// Mkdir 创建目录
func (c *Client) Mkdir(p string) error {
	ctx, cancel := metaCtx()
	defer cancel()
	resp, err := c.do(ctx, "MKCOL", c.fullURL(p), nil, -1, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webdav: MKCOL status %d", resp.StatusCode)
	}
	return nil
}

// Remove 删除
func (c *Client) Remove(p string) error {
	ctx, cancel := metaCtx()
	defer cancel()
	resp, err := c.do(ctx, "DELETE", c.fullURL(p), nil, -1, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webdav: DELETE status %d", resp.StatusCode)
	}
	return nil
}

// Rename 重命名/移动
func (c *Client) Rename(from, to string) error {
	ctx, cancel := metaCtx()
	defer cancel()
	resp, err := c.do(ctx, "MOVE", c.fullURL(from), nil, -1, map[string]string{
		"Destination": c.fullURL(to),
		"Overwrite":   "T",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("webdav: MOVE status %d: %s", resp.StatusCode, bytes.TrimSpace(buf))
	}
	return nil
}

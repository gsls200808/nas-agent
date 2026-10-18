// Package quark 夸克网盘客户端：基于 cookie 的 API 封装（分享解析/转存/下载）
// 以及扫码登录获取 cookie 的完整流程。
package quark

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// 域名常量
const (
	PanDomain    = "https://pan.quark.cn"      // 用户信息/登录态
	DriveDomain  = "https://drive-pc.quark.cn" // 主 API（转存/下载/删除）
	DriveHDomain = "https://drive-h.quark.cn"  // 分享页 API（token/列表）
)

// chromeUA 模拟浏览器请求
const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// clientUA 夸克 PC 客户端 UA：取下载地址和实际下载时使用。
// 官方对浏览器 UA 的大文件下载会返回 23018（download file size limit），
// 伪装客户端后可正常获取直链（参考 alist quark_uc 驱动、gopeed 夸克扩展）。
const clientUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"

// ShareFile 分享列表中的文件
type ShareFile struct {
	Fid           string
	ShareFidToken string
	Name          string
	Size          int64
	IsDir         bool
}

// Client 夸克网盘客户端
// 所有请求经 cookiejar 管理：初始 cookie 注入 jar，API 响应下发的
// 新 cookie（如 drive-h 域的 __puus）自动累积，下载时使用最新完整快照
type Client struct {
	http *http.Client
}

// NewClient 创建客户端，cookie 为完整 cookie 字符串
func NewClient(cookie string) *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{http: &http.Client{Timeout: 30 * time.Second, Jar: jar}}
	if cookie = strings.TrimSpace(cookie); cookie != "" {
		c.loadCookie(cookie)
	}
	return c
}

// loadCookie 将初始 cookie 注入 jar（.quark.cn 域共享，所有子域请求均携带）
func (c *Client) loadCookie(cookieStr string) {
	u, err := url.Parse(PanDomain)
	if err != nil {
		return
	}
	var cookies []*http.Cookie
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq <= 0 {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:   strings.TrimSpace(part[:eq]),
			Value:  strings.TrimSpace(part[eq+1:]),
			Domain: "quark.cn",
		})
	}
	if len(cookies) > 0 {
		c.http.Jar.SetCookies(u, cookies)
	}
}

// cookieHeader 从 jar 提取当前完整 cookie 快照（下载域校验需要最新完整 cookie）
func (c *Client) cookieHeader() string {
	u, err := url.Parse(PanDomain)
	if err != nil {
		return ""
	}
	cookies := c.http.Jar.Cookies(u)
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		if ck.Value == "" {
			continue
		}
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// request 发起 API 请求并解析 JSON 响应
// 返回完整响应 map；调用方自行校验 code/status
func (c *Client) request(method, rawURL string, body interface{}) (map[string]interface{}, error) {
	return c.requestWithUA(method, rawURL, body, chromeUA)
}

// requestWithUA 使用指定 User-Agent 发起 API 请求
func (c *Client) requestWithUA(method, rawURL string, body interface{}, ua string) (map[string]interface{}, error) {
	// drive 域名需附加通用 query 参数
	if strings.Contains(rawURL, "quark.cn/1/clouddrive") {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid url: %w", err)
		}
		q := u.Query()
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
		q.Set("uc_param_str", "")
		u.RawQuery = q.Encode()
		rawURL = u.String()
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewBuffer(data)
	}

	req, err := http.NewRequest(method, rawURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	// cookie 由 jar 自动携带（含响应更新），不手动设置 Cookie 头避免与 jar 重复
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", PanDomain+"/list")
	req.Header.Set("Origin", PanDomain)
	req.Header.Set("User-Agent", ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(data), 300))
	}

	var jsonResp map[string]interface{}
	if err := json.Unmarshal(data, &jsonResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return jsonResp, nil
}

// checkResp 校验响应 code==0 && status==200
// 注意：drive 域响应含 status 字段；pan 域（account/info）响应为 {code,data} 无 status
func checkResp(resp map[string]interface{}, action string) (map[string]interface{}, error) {
	code, _ := resp["code"].(float64)
	if int(code) != 0 {
		msg, _ := resp["message"].(string)
		if msg == "" {
			msg, _ = resp["errmsg"].(string)
		}
		return nil, fmt.Errorf("%s失败: code=%v %s", action, int(code), msg)
	}
	if st, ok := resp["status"].(float64); ok && int(st) != 200 {
		msg, _ := resp["message"].(string)
		return nil, fmt.Errorf("%s失败: status=%v %s", action, int(st), msg)
	}
	data, _ := resp["data"].(map[string]interface{})
	if data == nil {
		return nil, fmt.Errorf("%s失败: 响应缺少 data", action)
	}
	return data, nil
}

// GetUserInfo 获取用户信息（校验登录态），返回昵称
func (c *Client) GetUserInfo() (string, error) {
	resp, err := c.request("GET", PanDomain+"/account/info?fr=pc&platform=pc", nil)
	if err != nil {
		return "", fmt.Errorf("获取用户信息失败: %w", err)
	}
	data, err := checkResp(resp, "获取用户信息")
	if err != nil {
		return "", err
	}
	nickname, _ := data["nickname"].(string)
	return nickname, nil
}

// ParseShareText 从文本中提取分享链接 ID 与提取码
func ParseShareText(text string) (pwdID, passcode string, err error) {
	re := regexp.MustCompile(`/s/([0-9a-zA-Z]+)`)
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", "", fmt.Errorf("链接格式错误，未找到分享 ID")
	}
	pwdID = m[1]
	reCode := regexp.MustCompile(`提取码[:：]?\s*([0-9a-zA-Z]{1,10})`)
	if mc := reCode.FindStringSubmatch(text); len(mc) >= 2 {
		passcode = mc[1]
	}
	return pwdID, passcode, nil
}

// GetShareStoken 获取分享 stoken
func (c *Client) GetShareStoken(pwdID, passcode string) (string, error) {
	u := DriveHDomain + "/1/clouddrive/share/sharepage/token"
	resp, err := c.request("POST", u, map[string]interface{}{
		"pwd_id":                            pwdID,
		"passcode":                          passcode,
		"support_visit_limit_private_share": true,
	})
	if err != nil {
		return "", fmt.Errorf("获取分享凭证失败: %w", err)
	}
	data, err := checkResp(resp, "获取分享凭证")
	if err != nil {
		return "", err
	}
	stoken, _ := data["stoken"].(string)
	if stoken == "" {
		return "", fmt.Errorf("获取分享凭证失败: stoken 为空")
	}
	return stoken, nil
}

// GetShareList 获取分享文件列表（单页）
func (c *Client) GetShareList(pwdID, stoken, pdirFid string, page, size int) ([]ShareFile, error) {
	q := url.Values{}
	q.Set("pwd_id", pwdID)
	q.Set("stoken", stoken)
	q.Set("pdir_fid", pdirFid)
	q.Set("force", "0")
	q.Set("_page", fmt.Sprintf("%d", page))
	q.Set("_size", fmt.Sprintf("%d", size))
	q.Set("_fetch_banner", "1")
	q.Set("_fetch_share", "1")
	q.Set("_fetch_total", "1")
	q.Set("_sort", "file_type:asc,file_name:asc")
	q.Set("__dt", fmt.Sprintf("%d", 100+time.Now().UnixMilli()%900))
	q.Set("__t", fmt.Sprintf("%d", time.Now().UnixMilli()))

	resp, err := c.request("GET", DriveHDomain+"/1/clouddrive/share/sharepage/detail?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("获取分享列表失败: %w", err)
	}
	data, err := checkResp(resp, "获取分享列表")
	if err != nil {
		return nil, err
	}
	rawList, _ := data["list"].([]interface{})
	var files []ShareFile
	for _, item := range rawList {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fid, _ := obj["fid"].(string)
		name, _ := obj["file_name"].(string)
		if fid == "" || name == "" {
			continue
		}
		size, _ := obj["size"].(float64)
		ft, _ := obj["file_type"].(float64)
		token, _ := obj["share_fid_token"].(string)
		files = append(files, ShareFile{
			Fid:           fid,
			ShareFidToken: token,
			Name:          name,
			Size:          int64(size),
			IsDir:         int(ft) == 0, // 夸克: 0=文件夹 1=文件
		})
	}
	return files, nil
}

// SaveShareFile 转存文件到自己网盘根目录，返回新文件的 fid 列表
func (c *Client) SaveShareFile(pwdID, stoken string, fidList, shareTokenList []string) ([]string, error) {
	q := url.Values{}
	q.Set("__dt", fmt.Sprintf("%d", 100+time.Now().UnixMilli()%900))
	q.Set("__t", fmt.Sprintf("%d", time.Now().UnixMilli()))

	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/share/sharepage/save?"+q.Encode(), map[string]interface{}{
		"fid_list":         fidList,
		"share_token_list": shareTokenList,
		"to_pdir_fid":      "0",
		"pwd_id":           pwdID,
		"stoken":           stoken,
		"pdir_fid":         "0",
		"pdir_save_all":    false,
		"exclude_fids":     []string{},
		"scene":            "link",
	})
	if err != nil {
		return nil, fmt.Errorf("转存失败: %w", err)
	}
	data, err := checkResp(resp, "转存")
	if err != nil {
		return nil, err
	}
	taskID, _ := data["task_id"].(string)
	if taskID == "" {
		return nil, fmt.Errorf("转存失败: 未返回 task_id")
	}
	// 轮询任务直到完成，取 save_as.save_as_top_fids
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		tr, err := c.queryTask(taskID)
		if err != nil {
			return nil, err
		}
		if st, _ := tr["status"].(float64); int(st) == 2 {
			saveAs, _ := tr["save_as"].(map[string]interface{})
			if saveAs != nil {
				if fids, ok := saveAs["save_as_top_fids"].([]interface{}); ok {
					var out []string
					for _, f := range fids {
						if s, ok := f.(string); ok && s != "" {
							out = append(out, s)
						}
					}
					return out, nil
				}
			}
			return nil, fmt.Errorf("转存失败: 任务完成但未返回文件 ID")
		}
	}
	return nil, fmt.Errorf("转存超时")
}

// queryTask 查询任务状态
func (c *Client) queryTask(taskID string) (map[string]interface{}, error) {
	u := fmt.Sprintf("%s/1/clouddrive/task?task_id=%s&retry_index=0", DriveDomain, url.QueryEscape(taskID))
	resp, err := c.request("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("查询任务失败: %w", err)
	}
	data, err := checkResp(resp, "查询任务")
	if err != nil {
		return nil, err
	}
	return data, nil
}

// GetDownloadURL 获取文件下载地址（兼容同步/异步任务）
// 使用夸克 PC 客户端 UA，避免大文件被以 23018（download file size limit）拒绝
func (c *Client) GetDownloadURL(fid string) (string, error) {
	resp, err := c.requestWithUA("POST", DriveDomain+"/1/clouddrive/file/download", map[string]interface{}{
		"fids": []string{fid},
	}, clientUA)
	if err != nil {
		return "", fmt.Errorf("获取下载地址失败: %w", err)
	}
	code, _ := resp["code"].(float64)
	status, _ := resp["status"].(float64)
	if int(code) != 0 || int(status) != 200 {
		return "", fmt.Errorf("获取下载地址失败: code=%v status=%v", int(code), int(status))
	}
	rawData := resp["data"]
	// 同步：data 为数组，直接带 download_url
	if arr, ok := rawData.([]interface{}); ok && len(arr) > 0 {
		if first, ok := arr[0].(map[string]interface{}); ok {
			if u, _ := first["download_url"].(string); u != "" {
				return u, nil
			}
		}
	}
	// 异步：data 为对象，含 task_id / task_resp
	if obj, ok := rawData.(map[string]interface{}); ok {
		if taskResp, _ := obj["task_resp"].(map[string]interface{}); taskResp != nil {
			if arr, _ := taskResp["data"].([]interface{}); len(arr) > 0 {
				if first, _ := arr[0].(map[string]interface{}); first != nil {
					if u, _ := first["download_url"].(string); u != "" {
						return u, nil
					}
				}
			}
		}
		if taskID, _ := obj["task_id"].(string); taskID != "" {
			for i := 0; i < 30; i++ {
				time.Sleep(2 * time.Second)
				tr, err := c.queryTask(taskID)
				if err != nil {
					return "", err
				}
				st, _ := tr["status"].(float64)
				if int(st) == 3 {
					return "", fmt.Errorf("下载任务失败")
				}
				if int(st) == 2 {
					if u, _ := tr["download_url"].(string); u != "" {
						return u, nil
					}
					if arr, _ := tr["data"].([]interface{}); len(arr) > 0 {
						if first, _ := arr[0].(map[string]interface{}); first != nil {
							if u, _ := first["download_url"].(string); u != "" {
								return u, nil
							}
						}
					}
				}
			}
			return "", fmt.Errorf("获取下载地址超时")
		}
	}
	return "", fmt.Errorf("获取下载地址失败: 响应数据无效")
}

// DownloadFile 下载文件到本地路径（下载域校验需要完整 cookie 与浏览器头）
func (c *Client) DownloadFile(downloadURL, destPath string) error {
	w := &fileWriter{path: destPath}
	_, err := c.DownloadFileTo(downloadURL, w)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(destPath)
	}
	return err
}

// DownloadFileTo 下载文件写入任意 writer，返回写入字节数。
// 供调用方包装 writer 实现进度回调与取消（大文件下载不设总超时）。
func (c *Client) DownloadFileTo(downloadURL string, dst io.Writer) (int64, error) {
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	// 与取直链时保持同一客户端 UA（官方客户端下载校验）
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Referer", PanDomain+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	// 无论下载域名是否为 quark.cn 子域，都带上 jar 中最新的完整 cookie 快照
	if ch := c.cookieHeader(); ch != "" {
		req.Header.Set("Cookie", ch)
	}

	// 下载可能耗时较长，使用独立无超时客户端
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("下载请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	return io.Copy(dst, resp.Body)
}

// fileWriter 把写入落到本地文件（DownloadFile 的默认目标）
type fileWriter struct {
	path string
	f    *os.File
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if w.f == nil {
		f, err := os.Create(w.path)
		if err != nil {
			return 0, fmt.Errorf("创建本地文件失败: %w", err)
		}
		w.f = f
	}
	return w.f.Write(p)
}

func (w *fileWriter) Close() error {
	if w.f == nil {
		// 未写入任何数据时也要生成空文件
		f, err := os.Create(w.path)
		if err != nil {
			return fmt.Errorf("创建本地文件失败: %w", err)
		}
		return f.Close()
	}
	return w.f.Close()
}

// DeleteFiles 删除自己网盘中的文件（清理转存副本）
func (c *Client) DeleteFiles(fids []string) error {
	if len(fids) == 0 {
		return nil
	}
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/delete", map[string]interface{}{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     fids,
	})
	if err != nil {
		return fmt.Errorf("删除文件失败: %w", err)
	}
	if _, err := checkResp(resp, "删除文件"); err != nil {
		return err
	}
	return nil
}

// Capacity 网盘容量（字节）
type Capacity struct {
	Total int64 // 总容量
	Used  int64 // 已使用容量
}

// GetCapacity 查询网盘总容量与已用容量。
// 夸克不同版本接口字段位置不一致，这里按候选接口 + 递归查找字段做容错解析。
func (c *Client) GetCapacity() (*Capacity, error) {
	for _, ep := range []string{"/1/clouddrive/capacity", "/1/clouddrive/member"} {
		resp, err := c.request("GET", DriveDomain+ep, nil)
		if err != nil {
			continue
		}
		if cap := findCapacity(resp["data"]); cap != nil {
			return cap, nil
		}
	}
	return nil, fmt.Errorf("获取网盘容量失败")
}

// findCapacity 在响应数据中递归查找 total/used 容量字段
func findCapacity(v interface{}) *Capacity {
	var total, used int64
	var hasTotal, hasUsed bool
	var walk func(node interface{})
	walk = func(node interface{}) {
		obj, ok := node.(map[string]interface{})
		if !ok {
			if arr, ok := node.([]interface{}); ok {
				for _, item := range arr {
					walk(item)
				}
			}
			return
		}
		for k, val := range obj {
			switch strings.ToLower(k) {
			case "total_size", "total_capacity":
				if n, ok := val.(float64); ok && n > 0 && !hasTotal {
					total, hasTotal = int64(n), true
				}
			case "used_size", "use_capacity":
				if n, ok := val.(float64); ok && n >= 0 && !hasUsed {
					used, hasUsed = int64(n), true
				}
			}
			walk(val)
		}
	}
	walk(v)
	if hasTotal && hasUsed {
		return &Capacity{Total: total, Used: used}
	}
	return nil
}

// ---------- 上传（影视文件转存夸克） ----------

// MkdirAll 确保相对路径存在，返回最后一级文件夹的 fid。
// 夸克 mkdir API 用 dir_path 一次性创建整条路径（类似 mkdir -p），
// pdir_fid 固定 "0"、dir_init_lock=false；成功时 data.fid 即末级目录 fid。
// 已存在目录用 file/info/path_list 按完整路径查 fid。
func (c *Client) MkdirAll(relPath string) (string, error) {
	relPath = strings.Trim(strings.TrimSpace(relPath), "/")
	if relPath == "" {
		return "0", nil
	}
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file", map[string]interface{}{
		"pdir_fid":      "0",
		"file_name":     "",
		"dir_path":      relPath,
		"dir_init_lock": false,
	})
	if err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	if data, e := checkResp(resp, "创建目录"); e == nil {
		// 成功创建，data.fid 就是末级目录 fid
		if fid, _ := data["fid"].(string); fid != "" {
			return fid, nil
		}
	} else {
		// 失败：非"已存在/已创建"类错误直接返回
		msg := e.Error()
		if !strings.Contains(msg, "exist") && !strings.Contains(msg, "已存在") && !strings.Contains(msg, "已创建") {
			return "", e
		}
	}
	// 已存在或响应无 fid：用 path_list 按完整路径查末级 fid
	return c.getFidByPath(relPath)
}

// getFidByPath 用 file/info/path_list 按完整路径查询单个目录的 fid。
func (c *Client) getFidByPath(relPath string) (string, error) {
	target := "/" + strings.Trim(strings.TrimSpace(relPath), "/")
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/info/path_list", map[string]interface{}{
		"file_path": []string{target},
		"namespace": "0",
	})
	if err != nil {
		return "", fmt.Errorf("查询目录 fid 失败: %w", err)
	}
	// path_list 的 data 是数组 [{file_path, fid}, ...]，无法用 checkResp（它要 map）
	code, _ := resp["code"].(float64)
	if int(code) != 0 {
		msg, _ := resp["message"].(string)
		if msg == "" {
			msg, _ = resp["errmsg"].(string)
		}
		return "", fmt.Errorf("查询目录 fid 失败: %s", msg)
	}
	arr, ok := resp["data"].([]interface{})
	if !ok {
		return "", fmt.Errorf("查询目录 fid 失败: 响应无数组数据")
	}
	for _, item := range arr {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fp, _ := obj["file_path"].(string)
		if fid, _ := obj["fid"].(string); fid != "" && fp == target {
			return fid, nil
		}
	}
	return "", fmt.Errorf("目录 %s 不存在", target)
}

// ---------- 上传（影视文件转存夸克，走阿里云 OSS 分片上传） ----------

// OSS 上传常量。夸克上传实际是 OSS multipart：分片与合并的请求签名
// 由夸克 /1/clouddrive/file/upload/auth 接口按 auth_meta 字符串下发。
const (
	ossUserAgent   = "aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit"
	ossDateLayout  = "Mon, 02 Jan 2006 15:04:05 GMT"
	ossDefaultPart = 8 * 1024 * 1024
)

// upPreResp 上传预检响应（含 OSS 分片上传所需全部参数）
type upPreResp struct {
	Finish       bool   // true 表示秒传命中，无需真正上传
	Fid          string // 云端生成的目标文件 fid
	TaskID       string
	UploadID     string // OSS multipart upload_id
	AuthInfo     string // 换取 OSS 签名用的 auth_info
	UploadURL    string // OSS 端点（已去掉 scheme 前缀）
	ObjKey       string // OSS object key
	Bucket       string
	FormatType   string // MIME 类型
	PartSize     int64  // 官方建议分片大小
	CallbackURL  string
	CallbackBody string
}

// quarkCallback 上传回调（字段名需与 OSS 回调签名保持一致）
type quarkCallback struct {
	CallbackURL  string `json:"callbackUrl"`
	CallbackBody string `json:"callbackBody"`
}

// upPre 上传预检：POST /1/clouddrive/file/upload/pre
func (c *Client) upPre(name string, size int64, pdirFid string) (*upPreResp, error) {
	now := time.Now().UnixMilli()
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/upload/pre", map[string]interface{}{
		"file_name":       name,
		"size":            size,
		"pdir_fid":        pdirFid,
		"format_type":     mimeByExt(name),
		"ccp_hash_update": true,
		"l_created_at":    now,
		"l_updated_at":    now,
		"dir_name":        "",
		"parallel_upload": false,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化上传失败: %w", err)
	}
	data, err := checkResp(resp, "初始化上传")
	if err != nil {
		return nil, err
	}
	pre := &upPreResp{}
	pre.Finish, _ = data["finish"].(bool)
	pre.Fid, _ = data["fid"].(string)
	pre.TaskID, _ = data["task_id"].(string)
	pre.UploadID, _ = data["upload_id"].(string)
	pre.AuthInfo, _ = data["auth_info"].(string)
	pre.UploadURL, _ = data["upload_url"].(string)
	pre.ObjKey, _ = data["obj_key"].(string)
	pre.Bucket, _ = data["bucket"].(string)
	pre.FormatType, _ = data["format_type"].(string)
	if cb, ok := data["callback"].(map[string]interface{}); ok {
		pre.CallbackURL, _ = cb["callbackUrl"].(string)
		pre.CallbackBody, _ = cb["callbackBody"].(string)
	}
	if md, ok := resp["metadata"].(map[string]interface{}); ok {
		if ps, ok := md["part_size"].(float64); ok {
			pre.PartSize = int64(ps)
		}
	}
	// 后续按 {bucket}.{host} 拼接 URL，需去掉 scheme 前缀
	pre.UploadURL = strings.TrimPrefix(strings.TrimPrefix(pre.UploadURL, "https://"), "http://")
	return pre, nil
}

// upHash 提交 md5+sha1 做秒传校验：POST /1/clouddrive/file/update/hash
func (c *Client) upHash(md5sum, sha1sum, taskID string) (bool, error) {
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/update/hash", map[string]interface{}{
		"md5":     md5sum,
		"sha1":    sha1sum,
		"task_id": taskID,
	})
	if err != nil {
		return false, fmt.Errorf("校验文件 hash 失败: %w", err)
	}
	data, err := checkResp(resp, "校验文件 hash")
	if err != nil {
		return false, err
	}
	finish, _ := data["finish"].(bool)
	return finish, nil
}

// ossAuth 用 auth_meta 向夸克换取 OSS 签名（Authorization 头）：
// POST /1/clouddrive/file/upload/auth
func (c *Client) ossAuth(pre *upPreResp, authMeta string) (string, error) {
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/upload/auth", map[string]interface{}{
		"auth_info": pre.AuthInfo,
		"auth_meta": authMeta,
		"task_id":   pre.TaskID,
	})
	if err != nil {
		return "", fmt.Errorf("获取上传签名失败: %w", err)
	}
	data, err := checkResp(resp, "获取上传签名")
	if err != nil {
		return "", err
	}
	key, _ := data["auth_key"].(string)
	if key == "" {
		return "", fmt.Errorf("获取上传签名失败: auth_key 为空")
	}
	return key, nil
}

// upPart 上传单个分片到 OSS，返回分片 ETag
func (c *Client) upPart(pre *upPreResp, mimeType string, part int, data []byte) (string, error) {
	utc := time.Now().UTC().Format(ossDateLayout)
	authMeta := fmt.Sprintf("PUT\n\n%s\n%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?partNumber=%d&uploadId=%s",
		mimeType, utc, utc, ossUserAgent, pre.Bucket, pre.ObjKey, part, pre.UploadID)
	authKey, err := c.ossAuth(pre, authMeta)
	if err != nil {
		return "", err
	}
	// 与官方实现一致：分片 URL 形如 https://{bucket}.{host}//{objKey}
	partURL := fmt.Sprintf("https://%s.%s//%s?partNumber=%d&uploadId=%s",
		pre.Bucket, pre.UploadURL, pre.ObjKey, part, pre.UploadID)
	req, err := http.NewRequest("PUT", partURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", authKey)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("x-oss-date", utc)
	req.Header.Set("x-oss-user-agent", ossUserAgent)
	req.Header.Set("Referer", PanDomain+"/")
	req.Header.Set("Origin", PanDomain)
	req.Header.Set("User-Agent", chromeUA)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	etag := resp.Header.Get("Etag")
	if etag == "" {
		etag = resp.Header.Get("ETag")
	}
	if etag == "" {
		return "", fmt.Errorf("分片上传失败: 响应缺少 ETag")
	}
	return etag, nil
}

// upCommit 完成 OSS 分片上传（CompleteMultipartUpload）
func (c *Client) upCommit(pre *upPreResp, etags []string) error {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<CompleteMultipartUpload>\n")
	for i, e := range etags {
		fmt.Fprintf(&sb, "<Part>\n<PartNumber>%d</PartNumber>\n<ETag>%s</ETag>\n</Part>\n", i+1, e)
	}
	sb.WriteString("</CompleteMultipartUpload>")
	xmlBody := sb.String()

	sum := md5.Sum([]byte(xmlBody))
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	cbJSON, err := json.Marshal(quarkCallback{CallbackURL: pre.CallbackURL, CallbackBody: pre.CallbackBody})
	if err != nil {
		return fmt.Errorf("序列化上传回调失败: %w", err)
	}
	callbackBase64 := base64.StdEncoding.EncodeToString(cbJSON)

	utc := time.Now().UTC().Format(ossDateLayout)
	authMeta := fmt.Sprintf("POST\n%s\napplication/xml\n%s\nx-oss-callback:%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?uploadId=%s",
		contentMD5, utc, callbackBase64, utc, ossUserAgent, pre.Bucket, pre.ObjKey, pre.UploadID)
	authKey, err := c.ossAuth(pre, authMeta)
	if err != nil {
		return err
	}
	commitURL := fmt.Sprintf("https://%s.%s/%s?uploadId=%s", pre.Bucket, pre.UploadURL, pre.ObjKey, pre.UploadID)
	req, err := http.NewRequest("POST", commitURL, strings.NewReader(xmlBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authKey)
	req.Header.Set("Content-MD5", contentMD5)
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("x-oss-callback", callbackBase64)
	req.Header.Set("x-oss-date", utc)
	req.Header.Set("x-oss-user-agent", ossUserAgent)
	req.Header.Set("Referer", PanDomain+"/")
	req.Header.Set("Origin", PanDomain)
	req.Header.Set("User-Agent", chromeUA)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("完成分片上传失败: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return nil
}

// upFinish 通知夸克上传完成：POST /1/clouddrive/file/upload/finish
func (c *Client) upFinish(objKey, taskID string) error {
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/upload/finish", map[string]interface{}{
		"obj_key": objKey,
		"task_id": taskID,
	})
	if err != nil {
		return fmt.Errorf("提交上传失败: %w", err)
	}
	if _, err := checkResp(resp, "提交上传"); err != nil {
		return err
	}
	return nil
}

// mimeByExt 由文件扩展名推断 MIME 类型（与夸克客户端 mime_guess 行为对齐）
func mimeByExt(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".webm":
		return "video/webm"
	case ".m4v":
		return "video/x-m4v"
	case ".rmvb", ".rm":
		return "application/vnd.rn-realmedia"
	}
	return "application/octet-stream"
}

// UploadFile 将本地文件上传到夸克网盘指定目录，返回文件 fid。
// 流程：预检（秒传判断）→ 提交 hash → OSS 分片上传 → 合并 → finish。
func (c *Client) UploadFile(localPath, parentDirFid string) (string, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("读取本地文件失败: %w", err)
	}
	size := fi.Size()
	if size == 0 {
		return "", fmt.Errorf("文件为空")
	}
	name := path.Base(localPath)

	// 1. 上传预检
	pre, err := c.upPre(name, size, parentDirFid)
	if err != nil {
		return "", err
	}
	if pre.Finish {
		return pre.Fid, nil // 秒传命中，云端已有该文件
	}
	if pre.UploadID == "" {
		return "", fmt.Errorf("初始化上传失败: 响应缺少 upload_id")
	}

	// 2. 计算并提交文件 hash（命中则秒传，免实际上传）
	md5sum, err := fileMD5(localPath)
	if err != nil {
		return "", err
	}
	sha1sum, err := fileSHA1(localPath)
	if err != nil {
		return "", err
	}
	finish, err := c.upHash(md5sum, sha1sum, pre.TaskID)
	if err != nil {
		return "", err
	}
	if finish {
		return pre.Fid, nil
	}

	// 3. OSS 分片上传
	mimeType := pre.FormatType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	partSize := int(pre.PartSize)
	if partSize <= 0 {
		partSize = ossDefaultPart
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	buf := make([]byte, partSize)
	var etags []string
	for part := 1; ; part++ {
		n, rerr := io.ReadFull(f, buf)
		if n == 0 {
			break
		}
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return "", fmt.Errorf("读取分片 %d 失败: %w", part, rerr)
		}
		etag, err := c.upPart(pre, mimeType, part, buf[:n])
		if err != nil {
			return "", fmt.Errorf("上传分片 %d 失败: %w", part, err)
		}
		etags = append(etags, etag)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
	}

	// 4. 合并分片（CompleteMultipartUpload）
	if err := c.upCommit(pre, etags); err != nil {
		return "", err
	}

	// 5. 通知夸克上传完成
	if err := c.upFinish(pre.ObjKey, pre.TaskID); err != nil {
		return "", err
	}
	return pre.Fid, nil
}

// fileSHA1 计算文件 SHA1（流式，避免大文件占内存）
func fileSHA1(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileMD5 计算文件 MD5（流式，避免大文件占内存）
func fileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// truncate 截断字符串用于错误信息
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

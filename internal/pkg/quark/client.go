// Package quark 夸克网盘客户端：基于 cookie 的 API 封装（分享解析/转存/下载）
// 以及扫码登录获取 cookie 的完整流程。
package quark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
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
	// drive 域名需附加通用 query 参数
	if strings.Contains(rawURL, "quark.cn/1/clouddrive") {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid url: %w", err)
		}
		q := u.Query()
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
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
	req.Header.Set("User-Agent", chromeUA)
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
func (c *Client) GetDownloadURL(fid string) (string, error) {
	resp, err := c.request("POST", DriveDomain+"/1/clouddrive/file/download", map[string]interface{}{
		"fids": []string{fid},
	})
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
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", chromeUA)
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
		return fmt.Errorf("下载请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	return saveReaderToFile(resp.Body, destPath)
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

// saveReaderToFile 将流写入本地文件
func saveReaderToFile(rc io.Reader, destPath string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("创建本地文件失败: %w", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(destPath)
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return f.Close()
}

// truncate 截断字符串用于错误信息
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

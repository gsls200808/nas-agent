package quark

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// 扫码登录状态
const (
	LoginWaiting = "waiting" // 等待扫码/等待确认
	LoginSuccess = "success" // 登录成功
	LoginExpired = "expired" // 二维码过期
	LoginFailed  = "failed"  // 登录失败
)

// QRLoginStatus 扫码登录轮询结果
type QRLoginStatus struct {
	Status string // waiting / success / expired / failed
	Cookie string // 登录成功时的完整 cookie
}

// GetQRCodeToken 获取扫码登录 token 与二维码 URL
func GetQRCodeToken() (token, qrURL string, err error) {
	reqURL := fmt.Sprintf("https://uop.quark.cn/cas/ajax/getTokenForQrcodeLogin?client_id=532&v=1.2&request_id=%s",
		uuid.New().String())
	data, err := getJSON(reqURL)
	if err != nil {
		return "", "", fmt.Errorf("获取二维码 token 失败: %w", err)
	}
	if st, _ := data["status"].(float64); int(st) != 2000000 {
		msg, _ := data["message"].(string)
		return "", "", fmt.Errorf("获取二维码 token 失败: %s", msg)
	}
	members, _ := data["data"].(map[string]interface{})["members"].(map[string]interface{})
	if members == nil {
		return "", "", fmt.Errorf("获取二维码 token 失败: 响应缺少 members")
	}
	token, _ = members["token"].(string)
	if token == "" {
		return "", "", fmt.Errorf("获取二维码 token 失败: token 为空")
	}
	// 构造二维码 URL（夸克 APP 扫码登录页）
	q := url.Values{}
	q.Set("token", token)
	q.Set("client_id", "532")
	q.Set("ssb", "weblogin")
	q.Set("uc_param_str", "")
	q.Set("uc_biz_str", "S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0")
	qrURL = "https://su.quark.cn/4_eMHBJ?" + q.Encode()
	return token, qrURL, nil
}

// PollQRLogin 轮询扫码登录状态；成功时完成票据换取并返回完整 cookie
func PollQRLogin(token string) (*QRLoginStatus, error) {
	reqURL := fmt.Sprintf("https://uop.quark.cn/cas/ajax/getServiceTicketByQrcodeToken?client_id=532&v=1.2&token=%s&request_id=%s",
		url.QueryEscape(token), uuid.New().String())
	data, err := getJSON(reqURL)
	if err != nil {
		return nil, fmt.Errorf("查询扫码状态失败: %w", err)
	}
	st, _ := data["status"].(float64)
	switch int(st) {
	case 2000000:
		members, _ := data["data"].(map[string]interface{})["members"].(map[string]interface{})
		ticket, _ := members["service_ticket"].(string)
		if ticket == "" {
			return &QRLoginStatus{Status: LoginFailed}, nil
		}
		cookie, err := exchangeTicketForCookie(ticket)
		if err != nil {
			return nil, err
		}
		return &QRLoginStatus{Status: LoginSuccess, Cookie: cookie}, nil
	case 50004001:
		// 等待扫码 / 等待确认
		return &QRLoginStatus{Status: LoginWaiting}, nil
	case 50004002, 50004003, 50004004:
		// 过期 / 失败
		return &QRLoginStatus{Status: LoginExpired}, nil
	default:
		return &QRLoginStatus{Status: LoginFailed}, nil
	}
}

// exchangeTicketForCookie 用 service ticket 换取登录 cookie
// 请求 account/info 会经多次重定向并在每一跳通过 Set-Cookie 下发登录态，
// 必须使用 cookiejar 收集全部跳转的 cookie（Go 默认自动跟随重定向时会丢弃中间跳的 Set-Cookie）
func exchangeTicketForCookie(serviceTicket string) (string, error) {
	reqURL := fmt.Sprintf("%s/account/info?st=%s&lw=scan", PanDomain, url.QueryEscape(serviceTicket))
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("换取登录 cookie 失败: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("换取登录 cookie 失败: %w", err)
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", PanDomain+"/")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("换取登录 cookie 失败: %w", err)
	}
	defer resp.Body.Close()

	// 校验响应是否成功（票据无效时 cookie 不可用）
	body, _ := io.ReadAll(resp.Body)
	var jsonResp map[string]interface{}
	_ = json.Unmarshal(body, &jsonResp)
	if code, ok := jsonResp["code"].(float64); ok && int(code) != 0 {
		msg, _ := jsonResp["message"].(string)
		return "", fmt.Errorf("换取登录 cookie 失败: code=%d %s", int(code), msg)
	}

	// 从 jar 提取 pan.quark.cn 请求会携带的全部 cookie（含 .quark.cn 域）
	u, err := url.Parse(PanDomain)
	if err != nil {
		return "", fmt.Errorf("换取登录 cookie 失败: %w", err)
	}
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		return "", fmt.Errorf("换取登录 cookie 失败: 未收到 cookie")
	}
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	cookie := ""
	for i, p := range parts {
		if i > 0 {
			cookie += "; "
		}
		cookie += p
	}
	return cookie, nil
}

// getJSON 发起 GET 并解析 JSON（无需 cookie 的公共接口）
func getJSON(reqURL string) (map[string]interface{}, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var jsonResp map[string]interface{}
	if err := json.Unmarshal(body, &jsonResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return jsonResp, nil
}

package service

import (
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/dao"
	"nas-agent/internal/pkg/quark"
)

// QuarkService 夸克网盘服务（配置/扫码登录/分享转存下载）
type QuarkService struct {
	qrMu     sync.Mutex
	qrTokens map[int64]*quarkQRSession // userID → 当前扫码会话
}

// quarkQRSession 扫码登录会话
type quarkQRSession struct {
	token     string
	createdAt time.Time
}

// NewQuarkService 创建夸克服务
func NewQuarkService() *QuarkService {
	return &QuarkService{qrTokens: make(map[int64]*quarkQRSession)}
}

// QuarkStatus 夸克网盘绑定状态
type QuarkStatus struct {
	Configured bool   `json:"configured"` // 已配置 cookie
	Nickname   string `json:"nickname"`   // 网盘昵称
	Valid      bool   `json:"valid"`      // cookie 是否有效
}

// QuarkConfig 数据库模型
type QuarkConfig struct {
	ID        int64
	UserID    int64
	Cookie    string
	Nickname  string
	Status    string
	UpdatedAt time.Time
}

// getConfig 读取配置
func (s *QuarkService) getConfig(userID int64) (*QuarkConfig, error) {
	var cfg QuarkConfig
	err := dao.DB.QueryRow(
		`SELECT id, user_id, cookie, nickname, status, updated_at FROM quark_config WHERE user_id = ?`, userID,
	).Scan(&cfg.ID, &cfg.UserID, &cfg.Cookie, &cfg.Nickname, &cfg.Status, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// saveCookie 保存 cookie
func (s *QuarkService) saveCookie(userID int64, cookie, nickname, status string) error {
	_, err := dao.DB.Exec(
		`INSERT INTO quark_config(user_id, cookie, nickname, status, updated_at)
		 VALUES(?,?,?,?,datetime('now'))
		 ON CONFLICT(user_id) DO UPDATE SET
			cookie=excluded.cookie, nickname=excluded.nickname, status=excluded.status, updated_at=datetime('now')`,
		userID, cookie, nickname, status,
	)
	return err
}

// GetStatus 获取绑定状态（顺带校验 cookie 有效性）
func (s *QuarkService) GetStatus(userID int64) (*QuarkStatus, error) {
	cfg, err := s.getConfig(userID)
	if err != nil || cfg.Cookie == "" {
		return &QuarkStatus{Configured: false}, nil
	}
	st := &QuarkStatus{Configured: true, Nickname: cfg.Nickname, Valid: false}
	nickname, err := quark.NewClient(cfg.Cookie).GetUserInfo()
	if err == nil {
		st.Valid = true
		st.Nickname = nickname
		// 昵称有变化时更新
		if nickname != "" && nickname != cfg.Nickname {
			_ = s.saveCookie(userID, cfg.Cookie, nickname, "bound")
		}
	} else {
		// 诊断日志：校验失败原因 + 已存 cookie 的字段名（不含值）
		util.Logger.Warnf("夸克 cookie 校验失败（user=%d）: %v；已存 cookie 字段: %s",
			userID, err, cookieNames(cfg.Cookie))
	}
	return st, nil
}

// cookieNames 列出 cookie 字符串的字段名（用于诊断，不输出值）
func cookieNames(cookieStr string) string {
	var names []string
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.Index(part, "="); eq > 0 {
			names = append(names, part[:eq])
		}
	}
	return strings.Join(names, ",")
}

// SaveCookie 手动粘贴 cookie 保存
func (s *QuarkService) SaveCookie(userID int64, cookie string) error {
	cookie = strings.TrimSpace(cookie)
	cookie = strings.Trim(cookie, `"`)
	if cookie == "" {
		return fmt.Errorf("cookie 不能为空")
	}
	// 校验有效性并取昵称
	nickname, err := quark.NewClient(cookie).GetUserInfo()
	if err != nil {
		return fmt.Errorf("cookie 校验失败: %v", err)
	}
	return s.saveCookie(userID, cookie, nickname, "bound")
}

// Logout 解绑
func (s *QuarkService) Logout(userID int64) error {
	_, err := dao.DB.Exec(`DELETE FROM quark_config WHERE user_id = ?`, userID)
	return err
}

// GetQRCode 获取扫码登录二维码
func (s *QuarkService) GetQRCode(userID int64) (token, qrURL string, err error) {
	token, qrURL, err = quark.GetQRCodeToken()
	if err != nil {
		return "", "", err
	}
	s.qrMu.Lock()
	s.qrTokens[userID] = &quarkQRSession{token: token, createdAt: time.Now()}
	s.qrMu.Unlock()
	return token, qrURL, nil
}

// PollQRCode 轮询扫码登录状态；成功时保存 cookie
func (s *QuarkService) PollQRCode(userID int64, token string) (*quark.QRLoginStatus, error) {
	st, err := quark.PollQRLogin(token)
	if err != nil {
		return nil, err
	}
	if st.Status == quark.LoginSuccess && st.Cookie != "" {
		// 取昵称（失败不阻塞）
		nickname, _ := quark.NewClient(st.Cookie).GetUserInfo()
		if err := s.saveCookie(userID, st.Cookie, nickname, "bound"); err != nil {
			return nil, fmt.Errorf("保存 cookie 失败: %v", err)
		}
		// 清理会话
		s.qrMu.Lock()
		delete(s.qrTokens, userID)
		s.qrMu.Unlock()
	}
	return st, nil
}

// getClient 获取已配置的客户端
func (s *QuarkService) getClient(userID int64) (*quark.Client, error) {
	cfg, err := s.getConfig(userID)
	if err != nil || cfg.Cookie == "" {
		return nil, fmt.Errorf("夸克网盘未绑定，请先在网页端扫码登录或粘贴 cookie")
	}
	return quark.NewClient(cfg.Cookie), nil
}

// DownloadFromShare 解析分享链接并下载选中的文件到本地临时目录
// shareText 可包含提取码；keyword 用于在分享文件中挑选最匹配的音频文件
// 返回本地文件路径与文件名
func (s *QuarkService) DownloadFromShare(userID int64, shareText, keyword string) (string, string, error) {
	cli, err := s.getClient(userID)
	if err != nil {
		return "", "", err
	}

	pwdID, passcode, err := quark.ParseShareText(shareText)
	if err != nil {
		return "", "", err
	}
	stoken, err := cli.GetShareStoken(pwdID, passcode)
	if err != nil {
		return "", "", err
	}

	// 获取分享文件列表（根目录）
	files, err := cli.GetShareList(pwdID, stoken, "0", 1, 50)
	if err != nil {
		return "", "", err
	}
	if len(files) == 0 {
		return "", "", fmt.Errorf("分享内容为空")
	}
	target := pickAudioFile(cli, pwdID, stoken, files, keyword, 0)
	if target == nil {
		return "", "", fmt.Errorf("分享中没有找到音频文件")
	}

	// 转存到自己网盘
	fids, err := cli.SaveShareFile(pwdID, stoken, []string{target.Fid}, []string{target.ShareFidToken})
	if err != nil {
		return "", "", err
	}
	// 下载完成后清理云盘副本（无论下载是否成功都尝试清理）
	defer func() {
		_ = cli.DeleteFiles(fids)
	}()

	// 获取下载地址并下载
	downloadURL, err := cli.GetDownloadURL(fids[0])
	if err != nil {
		return "", "", err
	}
	ext := path.Ext(target.Name)
	if ext == "" {
		ext = ".mp3"
	}
	tmpPath := path.Join(os.TempDir(), fmt.Sprintf("nas-agent-quark-%d%s", time.Now().UnixNano(), ext))
	if err := cli.DownloadFile(downloadURL, tmpPath); err != nil {
		return "", "", err
	}
	return tmpPath, target.Name, nil
}

// pickAudioFile 从分享文件列表中挑选音频文件：
// 优先文件名包含关键词的；音质 FLAC 优先。列表中没有音频但有文件夹时递归进入（最多 3 层）。
func pickAudioFile(cli *quark.Client, pwdID, stoken string, files []quark.ShareFile, keyword string, depth int) *quark.ShareFile {
	kw := strings.ToLower(strings.TrimSpace(keyword))

	var audio []*quark.ShareFile
	for i := range files {
		f := &files[i]
		if !f.IsDir && audioExts[strings.ToLower(path.Ext(f.Name))] {
			audio = append(audio, f)
		}
	}
	if len(audio) == 0 {
		// 没有音频文件 → 进入第一个文件夹继续找
		if depth >= 3 {
			return nil
		}
		for _, f := range files {
			if !f.IsDir {
				continue
			}
			sub, err := cli.GetShareList(pwdID, stoken, f.Fid, 1, 50)
			if err != nil {
				continue
			}
			if hit := pickAudioFile(cli, pwdID, stoken, sub, keyword, depth+1); hit != nil {
				return hit
			}
		}
		return nil
	}

	best := audio[0]
	bestScore := -1
	for _, f := range audio {
		score := 0
		if kw != "" && strings.Contains(strings.ToLower(f.Name), kw) {
			score += 100
		}
		score += qualityScore(f.Name)
		if score > bestScore {
			bestScore = score
			best = f
		}
	}
	return best
}

// qualityScore 音质打分（无损优先）
func qualityScore(name string) int {
	switch {
	case strings.Contains(strings.ToLower(name), ".flac"):
		return 5
	case strings.Contains(strings.ToLower(name), ".wav"):
		return 4
	case strings.Contains(strings.ToLower(name), ".ape"):
		return 3
	case strings.Contains(strings.ToLower(name), ".m4a"):
		return 2
	default:
		return 1 // mp3 等
	}
}

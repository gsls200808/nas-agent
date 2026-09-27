package service

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/dao"
	"nas-agent/internal/pkg/clawbot"
)

// BotService 微信机器人服务
type BotService struct {
	musicSvc  *MusicService
	quarkSvc  *QuarkService
	searchSvc *MusicSearchService

	mu      sync.RWMutex
	running map[int64]context.CancelFunc // userID → cancel

	pendingMu sync.Mutex
	pending   map[string]*PendingSelection // 微信联系人 FromUserID → 待选歌曲列表
}

// PendingSong 待选歌曲（在线搜索结果）
type PendingSong struct {
	Index    int
	Title    string
	Artist   string
	Quality  string
	SearchID string // 全盘搜歌曲 ID
}

// PendingSelection 待选歌曲列表（10 分钟过期）
type PendingSelection struct {
	Songs     []PendingSong
	CreatedAt time.Time
}

// pendingExpiry 待选列表有效期
const pendingExpiry = 10 * time.Minute

// NewBotService 创建机器人服务
func NewBotService() *BotService {
	return &BotService{
		musicSvc:  NewMusicService(),
		quarkSvc:  NewQuarkService(),
		searchSvc: NewMusicSearchService(),
		running:   make(map[int64]context.CancelFunc),
		pending:   make(map[string]*PendingSelection),
	}
}

// setPending 保存待选列表
func (s *BotService) setPending(chatUserID string, songs []PendingSong) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending[chatUserID] = &PendingSelection{Songs: songs, CreatedAt: time.Now()}
}

// getPending 获取待选列表（过期自动清理）
func (s *BotService) getPending(chatUserID string) *PendingSelection {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	sel := s.pending[chatUserID]
	if sel == nil {
		return nil
	}
	if time.Since(sel.CreatedAt) > pendingExpiry {
		delete(s.pending, chatUserID)
		return nil
	}
	return sel
}

// takePending 取出并清除待选列表
func (s *BotService) takePending(chatUserID string) *PendingSelection {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	sel := s.pending[chatUserID]
	delete(s.pending, chatUserID)
	return sel
}

// BotStatus 机器人状态
type BotStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	BotID      string `json:"botId"`
	ILinkUserID string `json:"ilinkUserId"`
	Status     string `json:"status"` // online / offline / error
}

// GetStatus 获取机器人状态
func (s *BotService) GetStatus(userID int64) (*BotStatus, error) {
	cfg, err := s.getConfig(userID)
	if err != nil {
		return &BotStatus{LoggedIn: false, Status: "offline"}, nil
	}
	return &BotStatus{
		LoggedIn:    cfg.BotToken != "",
		BotID:       cfg.BotID,
		ILinkUserID: cfg.ILinkUserID,
		Status:      cfg.Status,
	}, nil
}

// BotConfig 机器人配置（数据库模型）
type BotConfig struct {
	ID          int64
	UserID      int64
	BotToken    string
	BaseURL     string
	BotID       string
	ILinkUserID string
	Status      string
	UpdatedAt   time.Time
}

// getConfig 从数据库读取配置
func (s *BotService) getConfig(userID int64) (*BotConfig, error) {
	var cfg BotConfig
	err := dao.DB.QueryRow(
		`SELECT id, user_id, bot_token, base_url, bot_id, ilink_user_id, status, updated_at
		 FROM bot_config WHERE user_id = ?`, userID,
	).Scan(&cfg.ID, &cfg.UserID, &cfg.BotToken, &cfg.BaseURL, &cfg.BotID, &cfg.ILinkUserID, &cfg.Status, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// saveConfig 保存配置
func (s *BotService) saveConfig(userID int64, creds clawbot.Credentials, status string) error {
	_, err := dao.DB.Exec(
		`INSERT INTO bot_config(user_id, bot_token, base_url, bot_id, ilink_user_id, status, updated_at)
		 VALUES(?,?,?,?,?,?,datetime('now'))
		 ON CONFLICT(user_id) DO UPDATE SET
			bot_token=excluded.bot_token, base_url=excluded.base_url, bot_id=excluded.bot_id,
			ilink_user_id=excluded.ilink_user_id, status=excluded.status, updated_at=datetime('now')`,
		userID, creds.BotToken, creds.BaseURL, creds.BotID, creds.UserID, status,
	)
	return err
}

// updateStatus 更新状态
func (s *BotService) updateStatus(userID int64, status string) error {
	_, err := dao.DB.Exec(
		`UPDATE bot_config SET status=?, updated_at=datetime('now') WHERE user_id=?`, status, userID,
	)
	return err
}

// FetchQRCode 获取登录二维码
func (s *BotService) FetchQRCode(userID int64) (clawbot.QRCodeResult, error) {
	// 尝试复用已有 base_url
	baseURL := ""
	if cfg, err := s.getConfig(userID); err == nil && cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return clawbot.FetchQRCode(ctx, baseURL)
}

// WaitForLogin 等待用户扫码确认
func (s *BotService) WaitForLogin(userID int64, qr clawbot.QRCodeResult) (*clawbot.Credentials, error) {
	baseURL := ""
	if cfg, err := s.getConfig(userID); err == nil && cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	creds, err := clawbot.WaitForLogin(ctx, baseURL, qr, clawbot.LoginCallbacks{})
	if err != nil {
		return nil, err
	}
	// 保存凭据
	if err := s.saveConfig(userID, creds, "online"); err != nil {
		return nil, fmt.Errorf("保存凭据失败: %v", err)
	}
	// 登录成功后自动拉起消息轮询（若已在运行则忽略）
	_ = s.Start(userID)
	return &creds, nil
}

// Start 启动机器人消息轮询
func (s *BotService) Start(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[userID]; ok {
		return fmt.Errorf("机器人已在运行")
	}
	cfg, err := s.getConfig(userID)
	if err != nil || cfg.BotToken == "" {
		return fmt.Errorf("机器人未登录")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.running[userID] = cancel

	go s.pollLoop(ctx, userID, clawbot.Credentials{
		BotToken: cfg.BotToken,
		BaseURL:  cfg.BaseURL,
		BotID:    cfg.BotID,
		UserID:   cfg.ILinkUserID,
	})
	return nil
}

// Stop 停止机器人
func (s *BotService) Stop(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.running[userID]; ok {
		cancel()
		delete(s.running, userID)
	}
	return s.updateStatus(userID, "offline")
}

// IsRunning 检查机器人是否在运行
func (s *BotService) IsRunning(userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.running[userID]
	return ok
}

// ResumeAll 服务启动时恢复所有已登录机器人（bot_token 非空）的消息轮询。
// 进程重启后内存中的轮询协程丢失，需依据数据库凭据重新拉起；
// 若凭据已过期，pollLoop 会在收到会话过期错误后自动标记 expired 并退出。
func (s *BotService) ResumeAll() {
	rows, err := dao.DB.Query(
		`SELECT user_id, bot_token, base_url, bot_id, ilink_user_id
		 FROM bot_config WHERE bot_token != ''`)
	if err != nil {
		util.Logger.Errorf("查询待恢复机器人失败: %v", err)
		return
	}
	type resumeItem struct {
		userID int64
		creds  clawbot.Credentials
	}
	var items []resumeItem
	for rows.Next() {
		var it resumeItem
		if err := rows.Scan(&it.userID, &it.creds.BotToken, &it.creds.BaseURL,
			&it.creds.BotID, &it.creds.UserID); err != nil {
			continue
		}
		items = append(items, it)
	}
	_ = rows.Close()

	for _, it := range items {
		s.mu.Lock()
		if _, ok := s.running[it.userID]; ok {
			s.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.running[it.userID] = cancel
		s.mu.Unlock()

		util.Logger.Infof("服务启动，自动恢复机器人 %d 的消息轮询", it.userID)
		go s.pollLoop(ctx, it.userID, it.creds)
	}
}

// pollLoop 消息轮询循环
func (s *BotService) pollLoop(ctx context.Context, userID int64, creds clawbot.Credentials) {
	buf := ""
	util.Logger.Infof("机器人 %d 开始轮询", userID)
	defer func() {
		s.mu.Lock()
		delete(s.running, userID)
		s.mu.Unlock()
		_ = s.updateStatus(userID, "offline")
		util.Logger.Infof("机器人 %d 停止轮询", userID)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		result, err := clawbot.Poll(ctx, creds, buf)
		if err != nil {
			if clawbot.IsSessionExpired(err) {
				util.Logger.Errorf("机器人 %d 会话过期，需要重新登录", userID)
				_ = s.updateStatus(userID, "expired")
				return
			}
			util.Logger.Errorf("机器人 %d 轮询错误: %v", userID, err)
			time.Sleep(3 * time.Second)
			continue
		}
		buf = result.GetUpdatesBuf

		for _, msg := range result.Messages {
			if msg.MessageType == clawbot.MessageTypeUser {
				go func(m *clawbot.WeixinMessage) {
					defer func() {
						if r := recover(); r != nil {
							util.Logger.Errorf("机器人 %d 处理消息 panic: %v", userID, r)
						}
					}()
					s.handleMessage(ctx, userID, creds, m)
				}(msg)
			}
		}
	}
}

// handleMessage 处理用户消息
func (s *BotService) handleMessage(ctx context.Context, userID int64, creds clawbot.Credentials, msg *clawbot.WeixinMessage) {
	text := strings.TrimSpace(msg.GetTextBody())
	target := clawbot.ReplyTarget{
		ToUserID:     msg.FromUserID,
		ContextToken: msg.ContextToken,
	}

	util.Logger.Infof("机器人 %d 收到消息 from=%s text=%q", userID, msg.FromUserID, text)

	// 指令解析
	switch {
	case text == "听音乐" || text == "听歌":
		s.handlePlayMusic(ctx, userID, creds, target, "")
	case strings.HasPrefix(text, "听音乐 ") || strings.HasPrefix(text, "听歌 "):
		keyword := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "听音乐 "), "听歌 "))
		s.handlePlayMusic(ctx, userID, creds, target, keyword)
	default:
		// 纯数字回复且有待选列表 → 选择歌曲下载
		if n, err := strconv.Atoi(text); err == nil && s.getPending(target.ToUserID) != nil {
			util.Logger.Infof("机器人 %d 收到序号选择 %d from=%s", userID, n, msg.FromUserID)
			s.handlePickSong(ctx, userID, creds, target, n)
			return
		}
		// 不认识的指令，不回复
	}
}

// handlePlayMusic 处理听音乐指令
func (s *BotService) handlePlayMusic(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, keyword string) {
	util.Logger.Infof("机器人 %d 开始处理听音乐指令 keyword=%q", userID, keyword)
	var song *SongInfo
	var err error

	t0 := time.Now()
	if keyword == "" {
		song, err = s.musicSvc.PickRandomSong(userID)
	} else {
		song, err = s.musicSvc.SearchSong(userID, keyword)
	}
	if err != nil {
		util.Logger.Errorf("机器人 %d 选歌失败（耗时 %s）: %v", userID, time.Since(t0), err)
		// 本地音乐目录没找到 → 尝试在线搜索（夸克网盘转存下载）
		if keyword != "" && s.tryOnlineSearch(ctx, userID, creds, target, keyword) {
			return
		}
		if e := clawbot.SendText(ctx, creds, target, err.Error()); e != nil {
			util.Logger.Errorf("机器人 %d 回复选歌失败提示也失败: %v", userID, e)
		}
		return
	}
	util.Logger.Infof("机器人 %d 选中歌曲（耗时 %s）: %s (%s, %d bytes)",
		userID, time.Since(t0), song.Name, song.Path, song.Size)

	// 先回复歌名
	if err := clawbot.SendText(ctx, creds, target, fmt.Sprintf("🎵 正在为您播放: %s", song.Name)); err != nil {
		util.Logger.Errorf("机器人 %d 发送播放提示文本失败: %v", userID, err)
	}

	// 下载歌曲
	t1 := time.Now()
	localPath, err := s.musicSvc.DownloadSong(song)
	if err != nil {
		util.Logger.Errorf("机器人 %d 下载歌曲失败（耗时 %s）: %v", userID, time.Since(t1), err)
		_ = clawbot.SendText(ctx, creds, target, "下载歌曲失败: "+err.Error())
		return
	}
	defer func() {
		// 延迟删除临时文件
		time.AfterFunc(5*time.Minute, func() {
			_ = os.Remove(localPath)
		})
	}()
	if fi, e := os.Stat(localPath); e == nil {
		util.Logger.Infof("机器人 %d 歌曲下载完成（耗时 %s）: %s (%d bytes)",
			userID, time.Since(t1), localPath, fi.Size())
	}

	// 发送音频文件
	t2 := time.Now()
	if err := clawbot.SendMediaByPath(ctx, creds, target, clawbot.MediaOpts{
		FilePath: localPath,
		Caption:  song.Name,
	}); err != nil {
		util.Logger.Errorf("机器人 %d 发送歌曲失败（耗时 %s）: %v", userID, time.Since(t2), err)
		_ = clawbot.SendText(ctx, creds, target, "发送歌曲失败: "+err.Error())
		_ = s.musicSvc.LogPlay(userID, target.ToUserID, song.Name, song.Path, song.ServerID, "send_failed")
		return
	}

	_ = s.musicSvc.LogPlay(userID, target.ToUserID, song.Name, song.Path, song.ServerID, "success")
	util.Logger.Infof("机器人 %d 发送歌曲成功（耗时 %s）: %s -> %s",
		userID, time.Since(t2), song.Name, target.ToUserID)
}

// tryOnlineSearch 本地没找到歌曲时，通过全盘搜在线查找并回复编号列表。
// 返回 true 表示已处理（已回复搜索结果或失败提示）；false 表示未处理（如夸克未绑定）。
func (s *BotService) tryOnlineSearch(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, keyword string) bool {
	st, err := s.quarkSvc.GetStatus(userID)
	if err != nil || !st.Configured {
		return false
	}
	util.Logger.Infof("机器人 %d 本地无「%s」，尝试在线搜索", userID, keyword)

	items, err := s.searchSvc.Search(keyword, 1, 5)
	if err != nil {
		util.Logger.Errorf("机器人 %d 在线搜索失败: %v", userID, err)
		_ = clawbot.SendText(ctx, creds, target, fmt.Sprintf("在线搜索失败: %v", err))
		return true
	}
	if len(items) == 0 {
		_ = clawbot.SendText(ctx, creds, target, fmt.Sprintf("本地音乐目录和网上都没有找到「%s」", keyword))
		return true
	}

	// 组装编号列表并保存待选状态
	songs := make([]PendingSong, 0, len(items))
	var sb strings.Builder
	fmt.Fprintf(&sb, "本地没有找到「%s」，网上搜索到以下歌曲，回复序号下载：\n", keyword)
	for i, it := range items {
		n := i + 1
		quality := ""
		if len(it.Quality) > 0 {
			quality = it.Quality[0]
		}
		songs = append(songs, PendingSong{
			Index: n, Title: it.Title, Artist: it.Artist, Quality: quality, SearchID: it.ID,
		})
		fmt.Fprintf(&sb, "%d %s - %s（%s）\n", n, it.Title, it.Artist, quality)
	}
	s.setPending(target.ToUserID, songs)
	_ = clawbot.SendText(ctx, creds, target, strings.TrimSpace(sb.String()))
	return true
}

// handlePickSong 处理序号选择：解析网盘分享链接 → 转存下载 → 发送 → 上传音乐目录
func (s *BotService) handlePickSong(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, n int) {
	sel := s.takePending(target.ToUserID)
	if sel == nil || n < 1 || n > len(sel.Songs) {
		_ = clawbot.SendText(ctx, creds, target, "序号无效，请重新发送「听音乐 歌名」搜索")
		return
	}
	song := sel.Songs[n-1]
	caption := song.Title
	if song.Artist != "" {
		caption = song.Title + " - " + song.Artist
	}
	util.Logger.Infof("机器人 %d 用户选择歌曲 %d: %s", userID, n, caption)

	_ = clawbot.SendText(ctx, creds, target, fmt.Sprintf("🎵 正在从网盘获取「%s」，请稍候…", caption))

	t0 := time.Now()
	// 获取网盘分享链接
	shareURL, _, err := s.searchSvc.GetShareURL(song.SearchID)
	if err != nil {
		util.Logger.Errorf("机器人 %d 获取分享链接失败: %v", userID, err)
		_ = clawbot.SendText(ctx, creds, target, "获取分享链接失败: "+err.Error())
		return
	}
	// 解析转存下载
	localPath, fileName, err := s.quarkSvc.DownloadFromShare(userID, shareURL, song.Title)
	if err != nil {
		util.Logger.Errorf("机器人 %d 网盘下载失败（耗时 %s）: %v", userID, time.Since(t0), err)
		_ = clawbot.SendText(ctx, creds, target, "网盘下载失败: "+err.Error())
		return
	}
	// 延迟删除临时文件
	defer time.AfterFunc(5*time.Minute, func() { _ = os.Remove(localPath) })
	if fi, e := os.Stat(localPath); e == nil {
		util.Logger.Infof("机器人 %d 网盘歌曲下载完成（耗时 %s）: %s (%d bytes)",
			userID, time.Since(t0), fileName, fi.Size())
	}

	// 发送音频文件
	if err := clawbot.SendMediaByPath(ctx, creds, target, clawbot.MediaOpts{
		FilePath: localPath,
		Caption:  caption,
	}); err != nil {
		util.Logger.Errorf("机器人 %d 发送网盘歌曲失败: %v", userID, err)
		_ = clawbot.SendText(ctx, creds, target, "发送歌曲失败: "+err.Error())
		return
	}
	_ = s.musicSvc.LogPlay(userID, target.ToUserID, caption, fileName, 0, "success")
	util.Logger.Infof("机器人 %d 发送网盘歌曲成功（耗时 %s）: %s", userID, time.Since(t0), caption)

	// 后台上传到 NAS 音乐目录（下次可直接本地播放）
	go s.uploadToMusicDir(userID, localPath, fileName)
}

// uploadToMusicDir 将下载的歌曲上传到用户配置的 NAS 音乐目录
func (s *BotService) uploadToMusicDir(userID int64, localPath, fileName string) {
	cfg, err := s.musicSvc.GetConfig(userID)
	if err != nil || cfg.ServerID == 0 {
		return
	}
	cli, _, err := openServerClient(cfg.ServerID)
	if err != nil {
		util.Logger.Errorf("机器人 %d 上传音乐目录失败: %v", userID, err)
		return
	}
	defer cli.Close()

	f, err := os.Open(localPath)
	if err != nil {
		util.Logger.Errorf("机器人 %d 上传音乐目录失败: %v", userID, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		util.Logger.Errorf("机器人 %d 上传音乐目录失败: %v", userID, err)
		return
	}
	remotePath := path.Join(cfg.MusicDir, fileName)
	if err := cli.Upload(remotePath, f, fi.Size()); err != nil {
		util.Logger.Errorf("机器人 %d 上传音乐目录失败: %v", userID, err)
		return
	}
	util.Logger.Infof("机器人 %d 歌曲已上传到音乐目录: %s", userID, remotePath)
}

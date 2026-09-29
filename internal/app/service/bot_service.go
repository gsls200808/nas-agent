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

// PendingSong 待选歌曲
type PendingSong struct {
	Index    int
	Title    string
	Artist   string
	Quality  string
	SearchID string     // 全盘搜歌曲 ID 或歌曲宝详情页 ID（网络菜单）
	Source   string     // 来源：xia（全盘搜）/ gequbao（歌曲宝）
	Local    *SongInfo  // 本地歌曲（本地菜单）
}

// PendingSelection 待选歌曲列表（10 分钟过期）
type PendingSelection struct {
	Songs     []PendingSong
	Type      string // local（本地交互菜单）/ web（网络搜索列表）
	Keyword   string // 原始关键词（本地菜单回复 0 时用于网络搜索）
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
func (s *BotService) setPending(chatUserID, selType, keyword string, songs []PendingSong) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending[chatUserID] = &PendingSelection{
		Songs:     songs,
		Type:      selType,
		Keyword:   keyword,
		CreatedAt: time.Now(),
	}
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
		// 纯数字回复且有待选列表 → 按列表类型处理
		if n, err := strconv.Atoi(text); err == nil {
			if sel := s.getPending(target.ToUserID); sel != nil {
				util.Logger.Infof("机器人 %d 收到序号选择 %d from=%s", userID, n, msg.FromUserID)
				if sel.Type == "local" {
					s.handleLocalPick(ctx, userID, creds, target, n)
				} else {
					s.handlePickSong(ctx, userID, creds, target, n)
				}
				return
			}
		}
		// 不认识的指令，不回复
	}
}

// handlePlayMusic 处理听音乐指令
func (s *BotService) handlePlayMusic(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, keyword string) {
	util.Logger.Infof("机器人 %d 开始处理听音乐指令 keyword=%q", userID, keyword)

	if keyword == "" {
		// 无关键词：随机播放
		t0 := time.Now()
		song, err := s.musicSvc.PickRandomSong(userID)
		if err != nil {
			util.Logger.Errorf("机器人 %d 随机选歌失败（耗时 %s）: %v", userID, time.Since(t0), err)
			_ = clawbot.SendText(ctx, creds, target, err.Error())
			return
		}
		s.sendLocalSong(ctx, userID, creds, target, song)
		return
	}

	// 有关键词：本地精确/模糊分类搜索
	t0 := time.Now()
	match, err := s.musicSvc.SearchLocal(userID, keyword)
	if err != nil {
		util.Logger.Errorf("机器人 %d 本地搜索失败（耗时 %s）: %v", userID, time.Since(t0), err)
		// 音乐目录未配置等错误 → 尝试在线搜索
		if s.tryOnlineSearch(ctx, userID, creds, target, keyword) {
			return
		}
		_ = clawbot.SendText(ctx, creds, target, err.Error())
		return
	}

	switch {
	case len(match.Exact) == 1:
		// 唯一精确匹配 → 直接播放
		s.sendLocalSong(ctx, userID, creds, target, match.Exact[0])
	case len(match.Exact) > 1:
		// 同名多首 → 交互菜单
		util.Logger.Infof("机器人 %d 「%s」本地精确匹配到 %d 首，进入交互菜单", userID, keyword, len(match.Exact))
		s.replyLocalMenu(ctx, creds, target, keyword, match.Exact)
	case len(match.Fuzzy) > 0:
		// 只有模糊匹配 → 交互菜单（避免搜「月光」直接播「白月光与朱砂痣」）
		util.Logger.Infof("机器人 %d 「%s」无精确匹配，模糊匹配到 %d 首，进入交互菜单", userID, keyword, len(match.Fuzzy))
		s.replyLocalMenu(ctx, creds, target, keyword, match.Fuzzy)
	default:
		// 本地完全没有 → 网络搜索
		if s.tryOnlineSearch(ctx, userID, creds, target, keyword) {
			return
		}
		_ = clawbot.SendText(ctx, creds, target, fmt.Sprintf("没有找到包含「%s」的歌曲（网络搜索需先在网页端绑定夸克网盘）", keyword))
	}
}

// songDisplayName 歌曲展示名：「歌名 - 歌手」
func songDisplayName(song *SongInfo) string {
	switch {
	case song.Artist != "":
		return song.Title + " - " + song.Artist
	case song.Title != "":
		return song.Title
	default:
		return song.Name
	}
}

// replyLocalMenu 回复本地歌曲交互菜单：1.播放 … 0.进入网络搜索
func (s *BotService) replyLocalMenu(ctx context.Context, creds clawbot.Credentials, target clawbot.ReplyTarget, keyword string, songs []*SongInfo) {
	pending := make([]PendingSong, 0, len(songs))
	var sb strings.Builder
	sb.WriteString("本地找到以下相关歌曲，回复序号播放，回复 0 进行网络搜索：")
	for i, sg := range songs {
		n := i + 1
		pending = append(pending, PendingSong{Index: n, Title: sg.Title, Artist: sg.Artist, Local: sg})
		fmt.Fprintf(&sb, "\n%d. 播放 %s", n, songDisplayName(sg))
	}
	sb.WriteString("\n0. 进入网络搜索")
	s.setPending(target.ToUserID, "local", keyword, pending)
	if err := clawbot.SendText(ctx, creds, target, sb.String()); err != nil {
		util.Logger.Errorf("发送本地菜单失败: %v", err)
	}
}

// handleLocalPick 处理本地菜单的序号回复
func (s *BotService) handleLocalPick(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, n int) {
	sel := s.getPending(target.ToUserID)
	if sel == nil {
		_ = clawbot.SendText(ctx, creds, target, "列表已过期，请重新发送「听音乐 歌名」搜索")
		return
	}
	// 0 → 进入网络搜索（用菜单保存的原始关键词）
	if n == 0 {
		keyword := sel.Keyword
		util.Logger.Infof("机器人 %d 用户选择网络搜索，关键词=%q", userID, keyword)
		if s.tryOnlineSearch(ctx, userID, creds, target, keyword) {
			return
		}
		_ = clawbot.SendText(ctx, creds, target, "网络搜索需先在网页端绑定夸克网盘")
		return
	}
	if n < 1 || n > len(sel.Songs) {
		_ = clawbot.SendText(ctx, creds, target, "序号无效，请回复列表中的序号（或回复 0 进行网络搜索）")
		return
	}
	// 有效序号 → 取出并清除菜单
	s.takePending(target.ToUserID)
	song := sel.Songs[n-1].Local
	if song == nil {
		_ = clawbot.SendText(ctx, creds, target, "序号无效，请重新发送「听音乐 歌名」搜索")
		return
	}
	util.Logger.Infof("机器人 %d 用户从本地菜单选择 %d: %s", userID, n, songDisplayName(song))
	s.sendLocalSong(ctx, userID, creds, target, song)
}

// sendLocalSong 下载 NAS 上的本地歌曲并发送到微信
func (s *BotService) sendLocalSong(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, song *SongInfo) {
	displayName := songDisplayName(song)
	util.Logger.Infof("机器人 %d 选中歌曲: %s (%s, %d bytes)", userID, displayName, song.Path, song.Size)

	// 先回复歌名
	if err := clawbot.SendText(ctx, creds, target, fmt.Sprintf("🎵 正在为您播放: %s", displayName)); err != nil {
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
		Caption:  displayName,
	}); err != nil {
		util.Logger.Errorf("机器人 %d 发送歌曲失败（耗时 %s）: %v", userID, time.Since(t2), err)
		_ = clawbot.SendText(ctx, creds, target, "发送歌曲失败: "+err.Error())
		_ = s.musicSvc.LogPlay(userID, target.ToUserID, displayName, song.Path, song.ServerID, "send_failed")
		return
	}

	_ = s.musicSvc.LogPlay(userID, target.ToUserID, displayName, song.Path, song.ServerID, "success")
	util.Logger.Infof("机器人 %d 发送歌曲成功（耗时 %s）: %s -> %s",
		userID, time.Since(t2), displayName, target.ToUserID)
}

// tryOnlineSearch 本地没找到歌曲时，通过全盘搜在线查找并回复编号列表。
// 返回 true 表示已处理（已回复搜索结果或失败提示）；false 表示未处理（如夸克未绑定）。
func (s *BotService) tryOnlineSearch(ctx context.Context, userID int64, creds clawbot.Credentials, target clawbot.ReplyTarget, keyword string) bool {
	st, err := s.quarkSvc.GetStatus(userID)
	if err != nil || !st.Configured {
		return false
	}
	util.Logger.Infof("机器人 %d 本地无「%s」，尝试在线搜索", userID, keyword)

	songs := make([]PendingSong, 0, 20)
	var sb strings.Builder
	fmt.Fprintf(&sb, "本地没有找到「%s」，网上搜索到以下歌曲，回复序号下载：\n", keyword)

	// 来源一：全盘搜（编号 1-10）
	items, err := s.searchSvc.Search(keyword, 1, 10)
	if err != nil {
		util.Logger.Errorf("机器人 %d 全盘搜失败: %v", userID, err)
	}
	for i, it := range items {
		n := i + 1
		quality := ""
		if len(it.Quality) > 0 {
			quality = it.Quality[0]
		}
		songs = append(songs, PendingSong{
			Index: n, Title: it.Title, Artist: it.Artist, Quality: quality, SearchID: it.ID, Source: "xia",
		})
		fmt.Fprintf(&sb, "%d %s - %s（%s）\n", n, it.Title, it.Artist, quality)
	}

	// 来源二：歌曲宝（编号固定从 11 开始，最多 10 条）
	gqItems, gerr := s.searchSvc.GequbaoSearch(keyword, 10)
	if gerr != nil {
		util.Logger.Errorf("机器人 %d 歌曲宝搜索失败: %v", userID, gerr)
	}
	for i, g := range gqItems {
		n := 11 + i
		title, artist := splitSongTitle(g.Title)
		songs = append(songs, PendingSong{
			Index: n, Title: title, Artist: artist, Quality: "歌曲宝", SearchID: g.ID, Source: "gequbao",
		})
		fmt.Fprintf(&sb, "%d %s - %s（歌曲宝）\n", n, title, artist)
	}

	if len(songs) == 0 {
		_ = clawbot.SendText(ctx, creds, target, fmt.Sprintf("本地音乐目录和网上都没有找到「%s」", keyword))
		return true
	}
	s.setPending(target.ToUserID, "web", keyword, songs)
	_ = clawbot.SendText(ctx, creds, target, strings.TrimSpace(sb.String()))
	return true
}

// splitSongTitle 拆分「歌名 - 歌手」格式标题
func splitSongTitle(s string) (title, artist string) {
	if i := strings.Index(s, " - "); i > 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+3:])
	}
	return s, ""
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
	// 按来源获取网盘分享链接（均为夸克分享，统一走转存下载）
	var shareURL string
	switch song.Source {
	case "gequbao":
		link, pwd, err := s.searchSvc.GequbaoQuarkLink(song.SearchID)
		if err != nil {
			util.Logger.Errorf("机器人 %d 获取歌曲宝分享链接失败: %v", userID, err)
			_ = clawbot.SendText(ctx, creds, target, "获取分享链接失败: "+err.Error())
			return
		}
		if pwd != "" {
			link += " 提取码：" + pwd
		}
		shareURL = link
	default:
		link, _, err := s.searchSvc.GetShareURL(song.SearchID)
		if err != nil {
			util.Logger.Errorf("机器人 %d 获取分享链接失败: %v", userID, err)
			_ = clawbot.SendText(ctx, creds, target, "获取分享链接失败: "+err.Error())
			return
		}
		shareURL = link
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

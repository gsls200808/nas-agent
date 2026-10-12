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
	"nas-agent/internal/pkg/qqbot"
)

// chatMessenger 会话消息发送抽象：每条入站消息构造一个实例。
// 微信实现包装 clawbot 轮询凭据与回复目标；QQ 实现包装 qqbot.Client 与被动回复 msg_id。
type chatMessenger interface {
	SendText(ctx context.Context, text string) error
	SendMedia(ctx context.Context, path, caption string) error
}

// wechatMessenger 微信消息发送
type wechatMessenger struct {
	creds  clawbot.Credentials
	target clawbot.ReplyTarget
}

func (m *wechatMessenger) SendText(ctx context.Context, text string) error {
	return clawbot.SendText(ctx, m.creds, m.target, text)
}

func (m *wechatMessenger) SendMedia(ctx context.Context, path, caption string) error {
	return clawbot.SendMediaByPath(ctx, m.creds, m.target, clawbot.MediaOpts{
		FilePath: path,
		Caption:  caption,
	})
}

// qqMessenger QQ 消息发送（同一 msg_id 的多条回复按 msg_seq 递增去重）
type qqMessenger struct {
	client *qqbot.Client
	chat   qqbot.ChatTarget
	msgID  string // 被动回复凭据（5 分钟有效，每条最多回复 5 条）
	mu     sync.Mutex
	seq    int
}

func (m *qqMessenger) nextSeq() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return m.seq
}

func (m *qqMessenger) SendText(ctx context.Context, text string) error {
	return m.client.SendText(ctx, m.chat, text, m.msgID, m.nextSeq())
}

func (m *qqMessenger) SendMedia(ctx context.Context, path, caption string) error {
	return m.client.SendFile(ctx, m.chat, path, caption, m.msgID, m.nextSeq())
}

// BotService 微信/QQ 机器人服务（一个系统用户可绑定多个账号）
type BotService struct {
	musicSvc  *MusicService
	quarkSvc  *QuarkService
	searchSvc *MusicSearchService
	videoSvc  *VideoService

	mu      sync.RWMutex
	running map[int64]context.CancelFunc // bot_account.id → cancel

	pendingMu sync.Mutex
	pending   map[string]*PendingSelection // key = accountID|chatKey → 待选列表
}

// PendingSong 待选歌曲
type PendingSong struct {
	Index    int
	Title    string
	Artist   string
	Quality  string
	SearchID string    // 全盘搜歌曲 ID 或歌曲宝详情页 ID（网络菜单）
	Source   string    // 来源：xia（全盘搜）/ gequbao（歌曲宝）
	Local    *SongInfo // 本地歌曲（本地菜单）
}

// PendingSelection 待选列表（10 分钟过期，音乐与影视共用）
type PendingSelection struct {
	Songs     []PendingSong // 音乐待选
	Type      string        // local（音乐本地）/ web（音乐网络）/ tv_show（选剧名）/ tv_episode（选集）/ movie（选电影）
	Keyword   string        // 原始关键词
	CreatedAt time.Time

	// 影视专用
	Show   *ShowFolder  // tv_episode：已选剧名
	Videos []*VideoFile // tv_show（剧名列表）/ tv_episode（剧集列表）/ movie（电影列表）
}

// pendingExpiry 待选列表有效期
const pendingExpiry = 10 * time.Minute

// 渠道常量
const (
	ChannelWechat = "wechat"
	ChannelQQ     = "qq"
)

// NewBotService 创建机器人服务
func NewBotService() *BotService {
	return &BotService{
		musicSvc:  NewMusicService(),
		quarkSvc:  NewQuarkService(),
		searchSvc: NewMusicSearchService(),
		videoSvc:  NewVideoService(),
		running:   make(map[int64]context.CancelFunc),
		pending:   make(map[string]*PendingSelection),
	}
}

// pendingKey 待选列表的复合键：不同账号的同一联系人/群互不干扰
func pendingKey(accountID int64, chatKey string) string {
	return strconv.FormatInt(accountID, 10) + "|" + chatKey
}

// setPending 保存待选列表
func (s *BotService) setPending(accountID int64, chatKey, selType, keyword string, songs []PendingSong) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending[pendingKey(accountID, chatKey)] = &PendingSelection{
		Songs:     songs,
		Type:      selType,
		Keyword:   keyword,
		CreatedAt: time.Now(),
	}
}

// getPending 获取待选列表（过期自动清理）
func (s *BotService) getPending(accountID int64, chatKey string) *PendingSelection {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	key := pendingKey(accountID, chatKey)
	sel := s.pending[key]
	if sel == nil {
		return nil
	}
	if time.Since(sel.CreatedAt) > pendingExpiry {
		delete(s.pending, key)
		return nil
	}
	return sel
}

// takePending 取出并清除待选列表
func (s *BotService) takePending(accountID int64, chatKey string) *PendingSelection {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	key := pendingKey(accountID, chatKey)
	sel := s.pending[key]
	delete(s.pending, key)
	return sel
}

// BotAccountStatus 机器人账号状态（前端展示）
type BotAccountStatus struct {
	ID          int64  `json:"id"`
	Channel     string `json:"channel"`
	Name        string `json:"name"`
	LoggedIn    bool   `json:"loggedIn"`
	BotID       string `json:"botId"`
	ILinkUserID string `json:"ilinkUserId"`
	AppID       string `json:"appId"`
	Sandbox     bool   `json:"sandbox"`
	Status      string `json:"status"` // online / offline / error / expired
	Running     bool   `json:"running"`
}

// ListAccounts 列出当前用户绑定的所有机器人账号
func (s *BotService) ListAccounts(userID int64) ([]BotAccountStatus, error) {
	accounts, err := s.listAccounts(userID)
	if err != nil {
		return nil, err
	}
	list := make([]BotAccountStatus, 0, len(accounts))
	for _, a := range accounts {
		loggedIn := a.BotToken != ""
		if a.Channel == ChannelQQ {
			loggedIn = a.AppID != "" && a.ClientSecret != ""
		}
		list = append(list, BotAccountStatus{
			ID:          a.ID,
			Channel:     a.Channel,
			Name:        a.Name,
			LoggedIn:    loggedIn,
			BotID:       a.BotID,
			ILinkUserID: a.ILinkUserID,
			AppID:       a.AppID,
			Sandbox:     a.Sandbox,
			Status:      a.Status,
			Running:     s.IsRunning(a.ID),
		})
	}
	return list, nil
}

// BotAccount 机器人账号配置（数据库模型）
type BotAccount struct {
	ID           int64
	UserID       int64
	Channel      string
	Name         string
	BotToken     string
	BaseURL      string
	BotID        string
	ILinkUserID  string
	AppID        string
	ClientSecret string
	Sandbox      bool
	Status       string
	UpdatedAt    time.Time
}

// listAccounts 查询用户的所有机器人账号
func (s *BotService) listAccounts(userID int64) ([]BotAccount, error) {
	rows, err := dao.DB.Query(
		`SELECT id, user_id, COALESCE(channel,'wechat'), name, bot_token, base_url, bot_id, ilink_user_id,
		        app_id, client_secret, sandbox, status, updated_at
		 FROM bot_account WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []BotAccount
	for rows.Next() {
		var a BotAccount
		if err := rows.Scan(&a.ID, &a.UserID, &a.Channel, &a.Name, &a.BotToken, &a.BaseURL,
			&a.BotID, &a.ILinkUserID, &a.AppID, &a.ClientSecret, &a.Sandbox, &a.Status, &a.UpdatedAt); err != nil {
			continue
		}
		list = append(list, a)
	}
	return list, nil
}

// getAccount 按 ID 读取机器人账号（校验归属）
func (s *BotService) getAccount(accountID, userID int64) (*BotAccount, error) {
	var a BotAccount
	err := dao.DB.QueryRow(
		`SELECT id, user_id, COALESCE(channel,'wechat'), name, bot_token, base_url, bot_id, ilink_user_id,
		        app_id, client_secret, sandbox, status, updated_at
		 FROM bot_account WHERE id = ? AND user_id = ?`, accountID, userID,
	).Scan(&a.ID, &a.UserID, &a.Channel, &a.Name, &a.BotToken, &a.BaseURL, &a.BotID, &a.ILinkUserID,
		&a.AppID, &a.ClientSecret, &a.Sandbox, &a.Status, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAccount 新建机器人账号绑定。
// 渠道 wechat：创建未登录账号，返回 ID 供扫码登录；
// 渠道 qq：appID/clientSecret 必填，校验 AppID 唯一后直接落库。
func (s *BotService) CreateAccount(userID int64, channel, name, appID, clientSecret string, sandbox bool) (int64, error) {
	if channel == "" {
		channel = ChannelWechat
	}
	if channel != ChannelWechat && channel != ChannelQQ {
		return 0, fmt.Errorf("不支持的渠道: %s", channel)
	}
	if channel == ChannelQQ {
		appID = strings.TrimSpace(appID)
		clientSecret = strings.TrimSpace(clientSecret)
		if appID == "" || clientSecret == "" {
			return 0, fmt.Errorf("请填写 QQ 开放平台的 AppID 与 AppSecret")
		}
		var cnt int
		if err := dao.DB.QueryRow(
			`SELECT COUNT(1) FROM bot_account WHERE channel='qq' AND app_id=?`, appID).Scan(&cnt); err != nil {
			return 0, err
		}
		if cnt > 0 {
			return 0, fmt.Errorf("该 AppID 已绑定过")
		}
		if strings.TrimSpace(name) == "" {
			name = "QQ机器人"
		}
		res, err := dao.DB.Exec(
			`INSERT INTO bot_account(user_id, name, channel, app_id, client_secret, sandbox, status)
			 VALUES(?,?,?,?,?,?,'offline')`, userID, name, channel, appID, clientSecret, sandbox)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}

	if strings.TrimSpace(name) == "" {
		name = "微信"
	}
	res, err := dao.DB.Exec(
		`INSERT INTO bot_account(user_id, name, channel, status) VALUES(?,?,?,'offline')`, userID, name, channel)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RenameAccount 修改账号备注名
func (s *BotService) RenameAccount(accountID, userID int64, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("备注名不能为空")
	}
	res, err := dao.DB.Exec(
		`UPDATE bot_account SET name=?, updated_at=datetime('now') WHERE id=? AND user_id=?`,
		name, accountID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("机器人账号不存在")
	}
	return nil
}

// UpdateAccount 更新账号信息：微信仅支持修改备注名；
// QQ 支持备注名、凭据编辑（appID/clientSecret 留空表示沿用旧值，sandbox 传 nil 表示不修改）。
func (s *BotService) UpdateAccount(accountID, userID int64, name, appID, clientSecret string, sandbox *bool) error {
	a, err := s.getAccount(accountID, userID)
	if err != nil {
		return fmt.Errorf("机器人账号不存在")
	}
	if name != "" && name != a.Name {
		if err := s.RenameAccount(accountID, userID, name); err != nil {
			return err
		}
	}
	if a.Channel != ChannelQQ {
		return nil
	}
	appID = strings.TrimSpace(appID)
	if appID != "" && appID != a.AppID {
		var cnt int
		if err := dao.DB.QueryRow(
			`SELECT COUNT(1) FROM bot_account WHERE channel='qq' AND app_id=? AND id != ?`, appID, accountID).Scan(&cnt); err != nil {
			return err
		}
		if cnt > 0 {
			return fmt.Errorf("该 AppID 已被其他账号绑定")
		}
		if _, err := dao.DB.Exec(`UPDATE bot_account SET app_id=?, updated_at=datetime('now') WHERE id=?`, appID, accountID); err != nil {
			return err
		}
	}
	if clientSecret != "" {
		if _, err := dao.DB.Exec(`UPDATE bot_account SET client_secret=?, updated_at=datetime('now') WHERE id=?`, clientSecret, accountID); err != nil {
			return err
		}
	}
	if sandbox != nil {
		if _, err := dao.DB.Exec(`UPDATE bot_account SET sandbox=?, updated_at=datetime('now') WHERE id=?`, *sandbox, accountID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteAccount 删除机器人账号绑定（先停运行再删行）
func (s *BotService) DeleteAccount(accountID, userID int64) error {
	if _, err := s.getAccount(accountID, userID); err != nil {
		return fmt.Errorf("机器人账号不存在")
	}
	_ = s.Stop(accountID)
	_, err := dao.DB.Exec(`DELETE FROM bot_account WHERE id=? AND user_id=?`, accountID, userID)
	return err
}

// saveAccountCreds 保存微信登录凭据
func (s *BotService) saveAccountCreds(accountID, userID int64, creds clawbot.Credentials, status string) error {
	_, err := dao.DB.Exec(
		`UPDATE bot_account SET bot_token=?, base_url=?, bot_id=?, ilink_user_id=?, status=?, updated_at=datetime('now')
		 WHERE id=? AND user_id=?`,
		creds.BotToken, creds.BaseURL, creds.BotID, creds.UserID, status, accountID, userID)
	return err
}

// updateStatus 更新账号状态
func (s *BotService) updateStatus(accountID int64, status string) error {
	_, err := dao.DB.Exec(
		`UPDATE bot_account SET status=?, updated_at=datetime('now') WHERE id=?`, status, accountID)
	return err
}

// FetchQRCode 获取指定微信账号的登录二维码
func (s *BotService) FetchQRCode(accountID, userID int64) (clawbot.QRCodeResult, error) {
	// 尝试复用账号已有 base_url
	baseURL := ""
	if a, err := s.getAccount(accountID, userID); err == nil {
		baseURL = a.BaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return clawbot.FetchQRCode(ctx, baseURL)
}

// WaitForLogin 等待用户扫码确认，成功后保存凭据并自动拉起轮询
func (s *BotService) WaitForLogin(accountID, userID int64, qr clawbot.QRCodeResult) (*clawbot.Credentials, error) {
	baseURL := ""
	if a, err := s.getAccount(accountID, userID); err == nil {
		baseURL = a.BaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	creds, err := clawbot.WaitForLogin(ctx, baseURL, qr, clawbot.LoginCallbacks{})
	if err != nil {
		return nil, err
	}
	// 保存凭据
	if err := s.saveAccountCreds(accountID, userID, creds, "online"); err != nil {
		return nil, fmt.Errorf("保存凭据失败: %v", err)
	}
	// 登录成功后自动拉起消息轮询（若已在运行则忽略）
	_ = s.Start(accountID, userID)
	return &creds, nil
}

// Start 启动指定账号（微信轮询 / QQ 网关）
func (s *BotService) Start(accountID, userID int64) error {
	a, err := s.getAccount(accountID, userID)
	if err != nil {
		return fmt.Errorf("机器人账号不存在")
	}

	var run func(ctx context.Context)
	switch a.Channel {
	case ChannelQQ:
		if a.AppID == "" || a.ClientSecret == "" {
			return fmt.Errorf("请先配置 QQ 开放平台 AppID 与 AppSecret")
		}
		client := qqbot.NewClient(a.AppID, a.ClientSecret, a.Sandbox)
		// 启动前校验凭据（拿一次 token）
		tctx, tcancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, terr := client.GetToken(tctx)
		tcancel()
		if terr != nil {
			return fmt.Errorf("QQ 凭据校验失败: %v", terr)
		}
		run = func(ctx context.Context) { s.qqLoop(ctx, accountID, userID, client) }
	default:
		if a.BotToken == "" {
			return fmt.Errorf("机器人未登录")
		}
		creds := clawbot.Credentials{
			BotToken: a.BotToken,
			BaseURL:  a.BaseURL,
			BotID:    a.BotID,
			UserID:   a.ILinkUserID,
		}
		run = func(ctx context.Context) { s.pollLoop(ctx, accountID, userID, creds) }
	}

	s.mu.Lock()
	if _, ok := s.running[accountID]; ok {
		s.mu.Unlock()
		return fmt.Errorf("机器人已在运行")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running[accountID] = cancel
	s.mu.Unlock()
	_ = s.updateStatus(accountID, "online")
	go run(ctx)
	return nil
}

// Stop 停止指定账号
func (s *BotService) Stop(accountID int64) error {
	s.mu.Lock()
	if cancel, ok := s.running[accountID]; ok {
		cancel()
		delete(s.running, accountID)
	}
	s.mu.Unlock()
	return s.updateStatus(accountID, "offline")
}

// IsRunning 检查指定账号是否在运行
func (s *BotService) IsRunning(accountID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.running[accountID]
	return ok
}

// ResumeAll 服务启动时恢复所有已配置账号：
// 微信（bot_token 非空）恢复消息轮询，QQ（app_id+client_secret 齐全）恢复网关连接。
// 进程重启后内存中的协程丢失，需依据数据库凭据重新拉起；
// 若微信凭据已过期，pollLoop 会在收到会话过期错误后自动标记 expired 并退出。
func (s *BotService) ResumeAll() {
	rows, err := dao.DB.Query(
		`SELECT id, user_id, COALESCE(channel,'wechat'), bot_token, base_url, bot_id, ilink_user_id,
		        app_id, client_secret, sandbox
		 FROM bot_account
		 WHERE bot_token != '' OR (COALESCE(channel,'wechat')='qq' AND app_id != '' AND client_secret != '')`)
	if err != nil {
		util.Logger.Errorf("查询待恢复机器人账号失败: %v", err)
		return
	}
	type resumeItem struct {
		accountID int64
		userID    int64
		channel   string
		creds     clawbot.Credentials
		appID     string
		secret    string
		sandbox   bool
	}
	var items []resumeItem
	for rows.Next() {
		var it resumeItem
		if err := rows.Scan(&it.accountID, &it.userID, &it.channel, &it.creds.BotToken, &it.creds.BaseURL,
			&it.creds.BotID, &it.creds.UserID, &it.appID, &it.secret, &it.sandbox); err != nil {
			continue
		}
		items = append(items, it)
	}
	_ = rows.Close()

	for _, it := range items {
		s.mu.Lock()
		if _, ok := s.running[it.accountID]; ok {
			s.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.running[it.accountID] = cancel
		s.mu.Unlock()

		if it.channel == ChannelQQ {
			util.Logger.Infof("服务启动，自动恢复 QQ 机器人 %d（AppID %s，用户 %d）的网关连接", it.accountID, it.appID, it.userID)
			_ = s.updateStatus(it.accountID, "online")
			client := qqbot.NewClient(it.appID, it.secret, it.sandbox)
			go s.qqLoop(ctx, it.accountID, it.userID, client)
			continue
		}
		util.Logger.Infof("服务启动，自动恢复微信账号 %d（用户 %d）的消息轮询", it.accountID, it.userID)
		_ = s.updateStatus(it.accountID, "online")
		go s.pollLoop(ctx, it.accountID, it.userID, it.creds)
	}
}

// pollLoop 微信消息轮询循环
func (s *BotService) pollLoop(ctx context.Context, accountID, userID int64, creds clawbot.Credentials) {
	buf := ""
	util.Logger.Infof("微信账号 %d 开始轮询", accountID)
	defer func() {
		s.mu.Lock()
		delete(s.running, accountID)
		s.mu.Unlock()
		_ = s.updateStatus(accountID, "offline")
		util.Logger.Infof("微信账号 %d 停止轮询", accountID)
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
				util.Logger.Errorf("微信账号 %d 会话过期，需要重新登录", accountID)
				_ = s.updateStatus(accountID, "expired")
				return
			}
			util.Logger.Errorf("微信账号 %d 轮询错误: %v", accountID, err)
			time.Sleep(3 * time.Second)
			continue
		}
		buf = result.GetUpdatesBuf

		for _, msg := range result.Messages {
			if msg.MessageType == clawbot.MessageTypeUser {
				msger := &wechatMessenger{
					creds: creds,
					target: clawbot.ReplyTarget{
						ToUserID:     msg.FromUserID,
						ContextToken: msg.ContextToken,
					},
				}
				go func(m *clawbot.WeixinMessage, mg chatMessenger) {
					defer func() {
						if r := recover(); r != nil {
							util.Logger.Errorf("微信账号 %d 处理消息 panic: %v", accountID, r)
						}
					}()
					s.handleMessage(ctx, accountID, userID, mg, m.FromUserID, m.GetTextBody())
				}(msg, msger)
			}
		}
	}
}

// qqLoop QQ 机器人网关循环（Client.Run 内部已带指数退避重连与 Resume）
func (s *BotService) qqLoop(ctx context.Context, accountID, userID int64, client *qqbot.Client) {
	util.Logger.Infof("QQ 机器人 %d 开始运行", accountID)
	defer func() {
		s.mu.Lock()
		delete(s.running, accountID)
		s.mu.Unlock()
		_ = s.updateStatus(accountID, "offline")
		util.Logger.Infof("QQ 机器人 %d 停止运行", accountID)
	}()

	err := client.Run(ctx, func(ev qqbot.Event) {
		if ev.Type != "C2C_MESSAGE_CREATE" && ev.Type != "GROUP_AT_MESSAGE_CREATE" {
			return
		}
		// 逐事件起协程处理，避免耗时业务阻塞网关 dispatch
		go func(ev qqbot.Event) {
			defer func() {
				if r := recover(); r != nil {
					util.Logger.Errorf("QQ 机器人 %d 处理消息 panic: %v", accountID, r)
				}
			}()
			text := strings.TrimSpace(ev.Content)
			util.Logger.Infof("QQ 机器人 %d 收到消息 chat=%s/%s sender=%s text=%q",
				accountID, ev.Chat.Kind, ev.Chat.OpenID, ev.Sender, text)
			msger := &qqMessenger{client: client, chat: ev.Chat, msgID: ev.MsgID, seq: 1}
			s.handleMessage(ctx, accountID, userID, msger, ev.Chat.OpenID, trimQQAtPrefix(text))
		}(ev)
	})
	if err != nil && ctx.Err() == nil {
		util.Logger.Errorf("QQ 机器人 %d 网关异常退出: %v", accountID, err)
	}
}

// trimQQAtPrefix 去除群聊消息中的 @机器人 前缀（<@!BOT_ID> / <@BOT_ID>）
func trimQQAtPrefix(text string) string {
	t := strings.TrimSpace(text)
	for strings.HasPrefix(t, "<@!") || strings.HasPrefix(t, "<@") {
		end := strings.Index(t, ">")
		if end < 0 {
			break
		}
		t = strings.TrimSpace(t[end+1:])
	}
	return t
}

// handleMessage 处理用户消息（微信/QQ 共用指令逻辑）
func (s *BotService) handleMessage(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey, text string) {
	util.Logger.Infof("机器人账号 %d 处理指令 from=%s text=%q", accountID, chatKey, text)

	// 指令解析
	switch {
	case text == "听音乐" || text == "听歌":
		s.handlePlayMusic(ctx, accountID, userID, msger, chatKey, "")
	case strings.HasPrefix(text, "听音乐 ") || strings.HasPrefix(text, "听歌 "):
		keyword := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "听音乐 "), "听歌 "))
		s.handlePlayMusic(ctx, accountID, userID, msger, chatKey, keyword)
	case text == "看电视" || text == "看电视剧":
		s.handleWatchTV(ctx, accountID, userID, msger, chatKey, "")
	case strings.HasPrefix(text, "看电视 ") || strings.HasPrefix(text, "看电视剧 "):
		keyword := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "看电视 "), "看电视剧 "))
		s.handleWatchTV(ctx, accountID, userID, msger, chatKey, keyword)
	case text == "看电影":
		s.handleWatchMovie(ctx, accountID, userID, msger, chatKey, "")
	case strings.HasPrefix(text, "看电影 "):
		keyword := strings.TrimSpace(strings.TrimPrefix(text, "看电影 "))
		s.handleWatchMovie(ctx, accountID, userID, msger, chatKey, keyword)
	default:
		// 纯数字回复且有待选列表 → 按列表类型处理
		if n, err := strconv.Atoi(text); err == nil {
			if sel := s.getPending(accountID, chatKey); sel != nil {
				util.Logger.Infof("机器人账号 %d 收到序号选择 %d from=%s type=%s", accountID, n, chatKey, sel.Type)
				switch sel.Type {
				case "local":
					s.handleLocalPick(ctx, accountID, userID, msger, chatKey, n)
				case "web":
					s.handlePickSong(ctx, accountID, userID, msger, chatKey, n)
				case "tv_show":
					s.handleTVShowPick(ctx, accountID, userID, msger, chatKey, n)
				case "tv_episode":
					s.handleTVEpisodePick(ctx, accountID, userID, msger, chatKey, n)
				case "movie":
					s.handleMoviePick(ctx, accountID, userID, msger, chatKey, n)
				}
				return
			}
		}
		// 不认识的指令，不回复
	}
}

// handlePlayMusic 处理听音乐指令
func (s *BotService) handlePlayMusic(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey, keyword string) {
	util.Logger.Infof("机器人账号 %d 开始处理听音乐指令 keyword=%q", accountID, keyword)

	if keyword == "" {
		// 无关键词：随机播放
		t0 := time.Now()
		song, err := s.musicSvc.PickRandomSong(userID)
		if err != nil {
			util.Logger.Errorf("机器人账号 %d 随机选歌失败（耗时 %s）: %v", accountID, time.Since(t0), err)
			_ = msger.SendText(ctx, err.Error())
			return
		}
		s.sendLocalSong(ctx, userID, msger, chatKey, song)
		return
	}

	// 有关键词：本地精确/模糊分类搜索
	t0 := time.Now()
	match, err := s.musicSvc.SearchLocal(userID, keyword)
	if err != nil {
		util.Logger.Errorf("机器人账号 %d 本地搜索失败（耗时 %s）: %v", accountID, time.Since(t0), err)
		// 音乐目录未配置等错误 → 尝试在线搜索
		if s.tryOnlineSearch(ctx, accountID, userID, msger, chatKey, keyword) {
			return
		}
		_ = msger.SendText(ctx, err.Error())
		return
	}

	switch {
	case len(match.Exact) == 1:
		// 唯一精确匹配 → 直接播放
		s.sendLocalSong(ctx, userID, msger, chatKey, match.Exact[0])
	case len(match.Exact) > 1:
		// 同名多首 → 交互菜单
		util.Logger.Infof("机器人账号 %d 「%s」本地精确匹配到 %d 首，进入交互菜单", accountID, keyword, len(match.Exact))
		s.replyLocalMenu(ctx, accountID, msger, chatKey, keyword, match.Exact)
	case len(match.Fuzzy) > 0:
		// 只有模糊匹配 → 交互菜单（避免搜「月光」直接播「白月光与朱砂痣」）
		util.Logger.Infof("机器人账号 %d 「%s」无精确匹配，模糊匹配到 %d 首，进入交互菜单", accountID, keyword, len(match.Fuzzy))
		s.replyLocalMenu(ctx, accountID, msger, chatKey, keyword, match.Fuzzy)
	default:
		// 本地完全没有 → 网络搜索
		if s.tryOnlineSearch(ctx, accountID, userID, msger, chatKey, keyword) {
			return
		}
		_ = msger.SendText(ctx, fmt.Sprintf("没有找到包含「%s」的歌曲（网络搜索需先在网页端绑定夸克网盘）", keyword))
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
func (s *BotService) replyLocalMenu(ctx context.Context, accountID int64, msger chatMessenger, chatKey, keyword string, songs []*SongInfo) {
	pending := make([]PendingSong, 0, len(songs))
	var sb strings.Builder
	sb.WriteString("本地找到以下相关歌曲，回复序号播放，回复 0 进行网络搜索：")
	for i, sg := range songs {
		n := i + 1
		pending = append(pending, PendingSong{Index: n, Title: sg.Title, Artist: sg.Artist, Local: sg})
		fmt.Fprintf(&sb, "\n%d. 播放 %s", n, songDisplayName(sg))
	}
	sb.WriteString("\n0. 进入网络搜索")
	s.setPending(accountID, chatKey, "local", keyword, pending)
	if err := msger.SendText(ctx, sb.String()); err != nil {
		util.Logger.Errorf("发送本地菜单失败: %v", err)
	}
}

// handleLocalPick 处理本地菜单的序号回复
func (s *BotService) handleLocalPick(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, n int) {
	sel := s.getPending(accountID, chatKey)
	if sel == nil {
		_ = msger.SendText(ctx, "列表已过期，请重新发送「听音乐 歌名」搜索")
		return
	}
	// 0 → 进入网络搜索（用菜单保存的原始关键词）
	if n == 0 {
		keyword := sel.Keyword
		util.Logger.Infof("机器人账号 %d 用户选择网络搜索，关键词=%q", accountID, keyword)
		if s.tryOnlineSearch(ctx, accountID, userID, msger, chatKey, keyword) {
			return
		}
		_ = msger.SendText(ctx, "网络搜索需先在网页端绑定夸克网盘")
		return
	}
	if n < 1 || n > len(sel.Songs) {
		_ = msger.SendText(ctx, "序号无效，请回复列表中的序号（或回复 0 进行网络搜索）")
		return
	}
	// 有效序号 → 取出并清除菜单
	s.takePending(accountID, chatKey)
	song := sel.Songs[n-1].Local
	if song == nil {
		_ = msger.SendText(ctx, "序号无效，请重新发送「听音乐 歌名」搜索")
		return
	}
	util.Logger.Infof("机器人账号 %d 用户从本地菜单选择 %d: %s", accountID, n, songDisplayName(song))
	s.sendLocalSong(ctx, userID, msger, chatKey, song)
}

// sendMediaOrExplain 发送媒体文件；QQ 渠道超过富媒体大小限制（HTTP 413，约 30MB）时
// 不回传网关 HTML 错误，改为向用户解释原因与替代方案。返回底层错误供调用方记日志。
func (s *BotService) sendMediaOrExplain(ctx context.Context, msger chatMessenger, localPath, caption string) error {
	err := msger.SendMedia(ctx, localPath, caption)
	if err == nil {
		return nil
	}
	if qqbot.IsPayloadTooLarge(err) {
		size := ""
		if fi, e := os.Stat(localPath); e == nil {
			size = fmt.Sprintf("%.1fMB，", float64(fi.Size())/1024/1024)
		}
		util.Logger.Errorf("文件超过 QQ 富媒体上传限制: %s%s", size, caption)
		_ = msger.SendText(ctx, fmt.Sprintf(
			"「%s」%s超过 QQ 机器人单文件约 15MB 的传输上限，无法通过 QQ 发送。可尝试点播该歌曲的 MP3 版本（体积更小），或通过微信机器人点播",
			caption, size))
		return err
	}
	_ = msger.SendText(ctx, "发送歌曲失败: "+err.Error())
	return err
}

// sendLocalSong 下载 NAS 上的本地歌曲并发送到会话
func (s *BotService) sendLocalSong(ctx context.Context, userID int64, msger chatMessenger, chatKey string, song *SongInfo) {
	displayName := songDisplayName(song)
	util.Logger.Infof("选中歌曲: %s (%s, %d bytes)", displayName, song.Path, song.Size)

	// 先回复歌名
	if err := msger.SendText(ctx, fmt.Sprintf("🎵 正在为您播放: %s", displayName)); err != nil {
		util.Logger.Errorf("发送播放提示文本失败: %v", err)
	}

	// 下载歌曲
	t1 := time.Now()
	localPath, err := s.musicSvc.DownloadSong(song)
	if err != nil {
		util.Logger.Errorf("下载歌曲失败（耗时 %s）: %v", time.Since(t1), err)
		_ = msger.SendText(ctx, "下载歌曲失败: "+err.Error())
		return
	}
	defer func() {
		// 延迟删除临时文件
		time.AfterFunc(5*time.Minute, func() {
			_ = os.Remove(localPath)
		})
	}()
	if fi, e := os.Stat(localPath); e == nil {
		util.Logger.Infof("歌曲下载完成（耗时 %s）: %s (%d bytes)",
			time.Since(t1), localPath, fi.Size())
	}

	// 发送音频文件
	t2 := time.Now()
	if err := s.sendMediaOrExplain(ctx, msger, localPath, displayName); err != nil {
		util.Logger.Errorf("发送歌曲失败（耗时 %s）: %v", time.Since(t2), err)
		_ = s.musicSvc.LogPlay(userID, chatKey, displayName, song.Path, song.ServerID, "send_failed")
		return
	}

	_ = s.musicSvc.LogPlay(userID, chatKey, displayName, song.Path, song.ServerID, "success")
	util.Logger.Infof("发送歌曲成功（耗时 %s）: %s -> %s",
		time.Since(t2), displayName, chatKey)
}

// tryOnlineSearch 本地没找到歌曲时，通过全盘搜在线查找并回复编号列表。
// 返回 true 表示已处理（已回复搜索结果或失败提示）；false 表示未处理（如夸克未绑定）。
func (s *BotService) tryOnlineSearch(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey, keyword string) bool {
	st, err := s.quarkSvc.GetStatus(userID)
	if err != nil || !st.Configured {
		return false
	}
	util.Logger.Infof("本地无「%s」，尝试在线搜索", keyword)

	songs := make([]PendingSong, 0, 20)
	var sb strings.Builder
	fmt.Fprintf(&sb, "本地没有找到「%s」，网上搜索到以下歌曲，回复序号下载：\n", keyword)

	// 来源一：全盘搜（编号 1-10）
	items, err := s.searchSvc.Search(keyword, 1, 10)
	if err != nil {
		util.Logger.Errorf("全盘搜失败: %v", err)
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
		util.Logger.Errorf("歌曲宝搜索失败: %v", gerr)
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
		_ = msger.SendText(ctx, fmt.Sprintf("本地音乐目录和网上都没有找到「%s」", keyword))
		return true
	}
	s.setPending(accountID, chatKey, "web", keyword, songs)
	_ = msger.SendText(ctx, strings.TrimSpace(sb.String()))
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
func (s *BotService) handlePickSong(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, n int) {
	sel := s.takePending(accountID, chatKey)
	if sel == nil || n < 1 || n > len(sel.Songs) {
		_ = msger.SendText(ctx, "序号无效，请重新发送「听音乐 歌名」搜索")
		return
	}
	song := sel.Songs[n-1]
	caption := song.Title
	if song.Artist != "" {
		caption = song.Title + " - " + song.Artist
	}
	util.Logger.Infof("机器人账号 %d 用户选择歌曲 %d: %s", accountID, n, caption)

	_ = msger.SendText(ctx, fmt.Sprintf("🎵 正在从网盘获取「%s」，请稍候…", caption))

	t0 := time.Now()
	// 按来源获取网盘分享链接（均为夸克分享，统一走转存下载）
	var shareURL string
	switch song.Source {
	case "gequbao":
		link, pwd, err := s.searchSvc.GequbaoQuarkLink(song.SearchID)
		if err != nil {
			util.Logger.Errorf("获取歌曲宝分享链接失败: %v", err)
			_ = msger.SendText(ctx, "获取分享链接失败: "+err.Error())
			return
		}
		if pwd != "" {
			link += " 提取码：" + pwd
		}
		shareURL = link
	default:
		link, _, err := s.searchSvc.GetShareURL(song.SearchID)
		if err != nil {
			util.Logger.Errorf("获取分享链接失败: %v", err)
			_ = msger.SendText(ctx, "获取分享链接失败: "+err.Error())
			return
		}
		shareURL = link
	}
	// 解析转存下载
	localPath, fileName, err := s.quarkSvc.DownloadFromShare(userID, shareURL, song.Title)
	if err != nil {
		util.Logger.Errorf("网盘下载失败（耗时 %s）: %v", time.Since(t0), err)
		_ = msger.SendText(ctx, "网盘下载失败: "+err.Error())
		return
	}
	// 延迟删除临时文件
	defer time.AfterFunc(5*time.Minute, func() { _ = os.Remove(localPath) })
	if fi, e := os.Stat(localPath); e == nil {
		util.Logger.Infof("网盘歌曲下载完成（耗时 %s）: %s (%d bytes)",
			time.Since(t0), fileName, fi.Size())
	}

	// 发送音频文件
	if err := s.sendMediaOrExplain(ctx, msger, localPath, caption); err != nil {
		util.Logger.Errorf("发送网盘歌曲失败: %v", err)
		return
	}
	_ = s.musicSvc.LogPlay(userID, chatKey, caption, fileName, 0, "success")
	util.Logger.Infof("发送网盘歌曲成功（耗时 %s）: %s", time.Since(t0), caption)

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
		util.Logger.Errorf("上传音乐目录失败: %v", err)
		return
	}
	defer cli.Close()

	f, err := os.Open(localPath)
	if err != nil {
		util.Logger.Errorf("上传音乐目录失败: %v", err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		util.Logger.Errorf("上传音乐目录失败: %v", err)
		return
	}
	remotePath := path.Join(cfg.MusicDir, fileName)
	if err := cli.Upload(remotePath, f, fi.Size()); err != nil {
		util.Logger.Errorf("上传音乐目录失败: %v", err)
		return
	}
	util.Logger.Infof("歌曲已上传到音乐目录: %s", remotePath)
}

// ---------- 影视（电视剧/电影）点播 ----------
// 与听音乐的区别：仅使用 NAS 配置目录，不走网络搜索；
// 视频文件通常远超机器人直传上限，统一上传到夸克网盘并提示路径。

// handleWatchTV 处理「看电视 剧名」指令
func (s *BotService) handleWatchTV(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey, keyword string) {
	util.Logger.Infof("机器人账号 %d 处理看电视指令 keyword=%q", accountID, keyword)

	if keyword == "" {
		shows, err := s.videoSvc.listShowFolders(userID)
		if err != nil {
			_ = msger.SendText(ctx, err.Error())
			return
		}
		if len(shows) == 0 {
			_ = msger.SendText(ctx, "电视剧目录下还没有电视剧文件夹，请先在网页端配置电视剧目录")
			return
		}
		s.replyShowMenu(ctx, accountID, msger, chatKey, shows)
		return
	}

	exact, fuzzy, err := s.videoSvc.SearchShows(userID, keyword)
	if err != nil {
		_ = msger.SendText(ctx, err.Error())
		return
	}
	switch {
	case len(exact) == 1:
		// 唯一精确匹配 → 直接列剧集
		s.handleShowSelected(ctx, accountID, userID, msger, chatKey, exact[0])
	case len(exact) > 1:
		s.replyShowMenu(ctx, accountID, msger, chatKey, exact)
	case len(fuzzy) > 0:
		s.replyShowMenu(ctx, accountID, msger, chatKey, fuzzy)
	default:
		_ = msger.SendText(ctx, fmt.Sprintf("电视剧目录中没有找到剧名包含「%s」的电视剧", keyword))
	}
}

// replyShowMenu 回复剧名选择菜单
func (s *BotService) replyShowMenu(ctx context.Context, accountID int64, msger chatMessenger, chatKey string, shows []*ShowFolder) {
	sel := &PendingSelection{
		Type:      "tv_show",
		Show:      nil,
		CreatedAt: time.Now(),
	}
	var sb strings.Builder
	sb.WriteString("找到以下电视剧，回复序号选择：")
	for i, sh := range shows {
		n := i + 1
		sel.Videos = append(sel.Videos, &VideoFile{Name: sh.Name, Path: sh.Path})
		fmt.Fprintf(&sb, "\n%d. %s", n, sh.Name)
	}
	s.setPendingSelection(accountID, chatKey, sel)
	if err := msger.SendText(ctx, sb.String()); err != nil {
		util.Logger.Errorf("发送剧名菜单失败: %v", err)
	}
}

// handleTVShowPick 处理剧名序号选择 → 列剧集
func (s *BotService) handleTVShowPick(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, n int) {
	sel := s.getPending(accountID, chatKey)
	if sel == nil || sel.Type != "tv_show" {
		_ = msger.SendText(ctx, "列表已过期，请重新发送「看电视 剧名」")
		return
	}
	if n < 1 || n > len(sel.Videos) {
		_ = msger.SendText(ctx, "序号无效，请回复列表中的序号")
		return
	}
	show := &ShowFolder{Name: sel.Videos[n-1].Name, Path: sel.Videos[n-1].Path}
	s.handleShowSelected(ctx, accountID, userID, msger, chatKey, show)
}

// handleShowSelected 列出指定剧名的所有剧集并回复菜单
func (s *BotService) handleShowSelected(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, show *ShowFolder) {
	eps, err := s.videoSvc.ListEpisodes(userID, show.Path)
	if err != nil {
		_ = msger.SendText(ctx, "列出剧集失败: "+err.Error())
		return
	}
	if len(eps) == 0 {
		_ = msger.SendText(ctx, fmt.Sprintf("「%s」目录下没有视频文件", show.Name))
		return
	}
	sel := &PendingSelection{
		Type:      "tv_episode",
		Show:      show,
		Videos:    eps,
		CreatedAt: time.Now(),
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "「%s」共 %d 集，回复序号播放（视频文件较大，将上传到夸克网盘）：", show.Name, len(eps))
	for i, ep := range eps {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, ep.Name)
	}
	s.setPendingSelection(accountID, chatKey, sel)
	if err := msger.SendText(ctx, sb.String()); err != nil {
		util.Logger.Errorf("发送剧集菜单失败: %v", err)
	}
}

// handleTVEpisodePick 处理剧集序号选择 → 上传夸克
func (s *BotService) handleTVEpisodePick(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, n int) {
	sel := s.takePending(accountID, chatKey)
	if sel == nil || sel.Type != "tv_episode" {
		_ = msger.SendText(ctx, "列表已过期，请重新发送「看电视 剧名」")
		return
	}
	if n < 1 || n > len(sel.Videos) {
		_ = msger.SendText(ctx, "序号无效，请回复列表中的序号")
		return
	}
	v := sel.Videos[n-1]
	title := sel.Show.Name + " - " + v.Name
	s.uploadVideoToQuark(ctx, userID, msger, v, title, sel.Show.Name)
}

// handleWatchMovie 处理「看电影 片名」指令
func (s *BotService) handleWatchMovie(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey, keyword string) {
	util.Logger.Infof("机器人账号 %d 处理看电影指令 keyword=%q", accountID, keyword)

	if keyword == "" {
		_ = msger.SendText(ctx, "请输入电影名，例如：看电影 流浪地球")
		return
	}

	exact, fuzzy, err := s.videoSvc.SearchMovies(userID, keyword)
	if err != nil {
		_ = msger.SendText(ctx, err.Error())
		return
	}
	switch {
	case len(exact) == 1:
		s.uploadVideoToQuark(ctx, userID, msger, exact[0], exact[0].Name, "电影")
	case len(exact) > 1 || len(fuzzy) > 0:
		sel := &PendingSelection{
			Type:      "movie",
			CreatedAt: time.Now(),
		}
		var all []*VideoFile
		all = append(all, exact...)
		all = append(all, fuzzy...)
		sel.Videos = all
		var sb strings.Builder
		sb.WriteString("找到以下电影，回复序号选择（视频文件较大，将上传到夸克网盘）：")
		for i, mv := range all {
			fmt.Fprintf(&sb, "\n%d. %s", i+1, mv.Name)
		}
		s.setPendingSelection(accountID, chatKey, sel)
		if err := msger.SendText(ctx, sb.String()); err != nil {
			util.Logger.Errorf("发送电影菜单失败: %v", err)
		}
	default:
		_ = msger.SendText(ctx, fmt.Sprintf("电影目录中没有找到片名包含「%s」的电影", keyword))
	}
}

// handleMoviePick 处理电影序号选择 → 上传夸克
func (s *BotService) handleMoviePick(ctx context.Context, accountID, userID int64, msger chatMessenger, chatKey string, n int) {
	sel := s.takePending(accountID, chatKey)
	if sel == nil || sel.Type != "movie" {
		_ = msger.SendText(ctx, "列表已过期，请重新发送「看电影 片名」")
		return
	}
	if n < 1 || n > len(sel.Videos) {
		_ = msger.SendText(ctx, "序号无效，请回复列表中的序号")
		return
	}
	v := sel.Videos[n-1]
	s.uploadVideoToQuark(ctx, userID, msger, v, v.Name, "电影")
}

// uploadVideoToQuark 下载 NAS 视频文件并上传到夸克网盘，回复保存路径。
// category 用于夸克目录归类（剧名或「电影」）。
func (s *BotService) uploadVideoToQuark(ctx context.Context, userID int64, msger chatMessenger, v *VideoFile, title, category string) {
	// 检查夸克是否已绑定
	st, err := s.quarkSvc.GetStatus(userID)
	if err != nil || !st.Configured || !st.Valid {
		_ = msger.SendText(ctx, "视频文件较大，需上传到夸克网盘。请先在网页端绑定夸克网盘（扫码或粘贴 cookie）")
		return
	}

	_ = msger.SendText(ctx, fmt.Sprintf("🎬 正在为您准备「%s」，先从 NAS 下载再上传夸克网盘，请稍候…", title))

	// 1. 从 NAS 下载到本地临时目录
	t1 := time.Now()
	localPath, err := s.videoSvc.DownloadVideo(v)
	if err != nil {
		util.Logger.Errorf("下载视频失败: %v", err)
		_ = msger.SendText(ctx, "下载视频失败: "+err.Error())
		return
	}
	defer time.AfterFunc(30*time.Minute, func() { _ = os.Remove(localPath) })

	if fi, e := os.Stat(localPath); e == nil {
		util.Logger.Infof("视频下载完成（耗时 %s）: %s (%.1fMB)",
			time.Since(t1), v.FullName, float64(fi.Size())/1024/1024)
		_ = msger.SendText(ctx, fmt.Sprintf("视频已下载到本地（%.1fMB），正在上传到夸克网盘…", float64(fi.Size())/1024/1024))
	}

	// 2. 上传到夸克网盘：影视/<category>/<filename>
	// 上传时用的是本地文件（临时名 nas-agent-video-xxx），夸克里的文件名与之相同，
	// 因此消息路径也按实际上传的文件名回显，避免与实际不符。
	quarkDir := "影视/" + category
	quarkPath := quarkDir + "/" + path.Base(localPath)
	util.Logger.Infof("开始上传视频到夸克: %s", quarkPath)

	t2 := time.Now()
	if err := s.quarkSvc.UploadFile(userID, localPath, quarkDir); err != nil {
		util.Logger.Errorf("上传夸克网盘失败（耗时 %s）: %v", time.Since(t2), err)
		_ = msger.SendText(ctx, "上传到夸克网盘失败: "+err.Error())
		return
	}

	util.Logger.Infof("视频已上传到夸克网盘（耗时 %s）: %s", time.Since(t2), quarkPath)
	_ = msger.SendText(ctx, fmt.Sprintf(
		"✅ 「%s」已上传到夸克网盘\n路径：%s\n请在夸克网盘 App/网页中查看播放。", title, quarkPath))
}

// setPendingSelection 保存通用待选列表（含影视）
func (s *BotService) setPendingSelection(accountID int64, chatKey string, sel *PendingSelection) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	sel.CreatedAt = time.Now()
	s.pending[pendingKey(accountID, chatKey)] = sel
}

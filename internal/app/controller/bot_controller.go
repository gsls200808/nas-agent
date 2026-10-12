package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/middleware"
	"nas-agent/internal/app/service"
	"nas-agent/internal/pkg/clawbot"
)

// BotController 机器人控制器
type BotController struct {
	svc      *service.BotService
	musicSvc *service.MusicService
	quarkSvc *service.QuarkService
	videoSvc *service.VideoService
}

// NewBotController 创建机器人控制器
func NewBotController() *BotController {
	return &BotController{
		svc:      service.NewBotService(),
		musicSvc: service.NewMusicService(),
		quarkSvc: service.NewQuarkService(),
		videoSvc: service.NewVideoService(),
	}
}

// parseAccountID 解析路由参数中的账号 ID 并校验归属
func (b *BotController) parseAccountID(c *gin.Context) (int64, int64, bool) {
	userID := middleware.CurrentUserID(c)
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		rsp.Fail(c, "账号 ID 无效")
		return 0, 0, false
	}
	return accountID, userID, true
}

// Accounts GET /api/bot/accounts — 列出当前用户绑定的微信账号
func (b *BotController) Accounts(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	list, err := b.svc.ListAccounts(userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, list)
}

// CreateAccount POST /api/bot/account — 新建账号绑定（微信扫码 / QQ 凭据）
func (b *BotController) CreateAccount(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		Name         string `json:"name"`
		Channel      string `json:"channel"`
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
		Sandbox      bool   `json:"sandbox"`
	}
	_ = c.ShouldBindJSON(&req) // 各字段均可选，渠道默认微信
	id, err := b.svc.CreateAccount(userID, req.Channel, req.Name, req.AppID, req.ClientSecret, req.Sandbox)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"id": id})
}

// UpdateAccount PUT /api/bot/account/:id — 修改账号（备注名 / QQ 凭据编辑）
func (b *BotController) UpdateAccount(c *gin.Context) {
	accountID, userID, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	var req struct {
		Name         string `json:"name"`
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
		Sandbox      *bool  `json:"sandbox"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := b.svc.UpdateAccount(accountID, userID, req.Name, req.AppID, req.ClientSecret, req.Sandbox); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// DeleteAccount DELETE /api/bot/account/:id — 删除微信账号绑定
func (b *BotController) DeleteAccount(c *gin.Context) {
	accountID, userID, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	if err := b.svc.DeleteAccount(accountID, userID); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// ResumeOnStartup 服务启动时恢复所有已登录微信账号的消息轮询
func (b *BotController) ResumeOnStartup() {
	b.svc.ResumeAll()
}

// QRCode POST /api/bot/login/qrcode — 获取指定账号的登录二维码
func (b *BotController) QRCode(c *gin.Context) {
	accountID, userID, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	qr, err := b.svc.FetchQRCode(accountID, userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, qr)
}

// ConfirmLogin POST /api/bot/login/confirm — 等待扫码确认并保存凭据
func (b *BotController) ConfirmLogin(c *gin.Context) {
	accountID, userID, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	var req struct {
		QRCode           string `json:"qrcode"`
		QRCodeImgContent string `json:"qrcodeImgContent"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	qr := clawbot.QRCodeResult{QRCode: req.QRCode, QRCodeImgContent: req.QRCodeImgContent}
	creds, err := b.svc.WaitForLogin(accountID, userID, qr)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, creds)
}

// Start POST /api/bot/account/:id/start — 启动消息轮询
func (b *BotController) Start(c *gin.Context) {
	accountID, userID, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	if err := b.svc.Start(accountID, userID); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// Stop POST /api/bot/account/:id/stop — 停止消息轮询
func (b *BotController) Stop(c *gin.Context) {
	accountID, _, ok := b.parseAccountID(c)
	if !ok {
		return
	}
	if err := b.svc.Stop(accountID); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// GetMusicConfig GET /api/bot/music/config
func (b *BotController) GetMusicConfig(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	cfg, err := b.musicSvc.GetConfig(userID)
	if err != nil {
		rsp.OK(c, gin.H{"serverId": 0, "musicDir": "/"})
		return
	}
	rsp.OK(c, cfg)
}

// SaveMusicConfig POST /api/bot/music/config
func (b *BotController) SaveMusicConfig(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		ServerID int64  `json:"serverId"`
		MusicDir string `json:"musicDir"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := b.musicSvc.SaveConfig(userID, req.ServerID, req.MusicDir); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// GetVideoConfig GET /api/bot/video/config
func (b *BotController) GetVideoConfig(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	cfg, err := b.videoSvc.GetConfig(userID)
	if err != nil {
		rsp.OK(c, gin.H{"tvServerId": 0, "tvDir": "/", "movieServerId": 0, "movieDir": "/"})
		return
	}
	rsp.OK(c, cfg)
}

// SaveVideoConfig POST /api/bot/video/config
func (b *BotController) SaveVideoConfig(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		TvServerID    int64  `json:"tvServerId"`
		TvDir         string `json:"tvDir"`
		MovieServerID int64  `json:"movieServerId"`
		MovieDir      string `json:"movieDir"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := b.videoSvc.SaveConfig(userID, req.TvServerID, req.TvDir, req.MovieServerID, req.MovieDir); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// QuarkStatus GET /api/bot/quark/status
func (b *BotController) QuarkStatus(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	st, err := b.quarkSvc.GetStatus(userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, st)
}

// QuarkSaveCookie POST /api/bot/quark/cookie
func (b *BotController) QuarkSaveCookie(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := b.quarkSvc.SaveCookie(userID, req.Cookie); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// QuarkLogout POST /api/bot/quark/logout
func (b *BotController) QuarkLogout(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	if err := b.quarkSvc.Logout(userID); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// QuarkQRCode GET /api/bot/quark/qrcode
func (b *BotController) QuarkQRCode(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	token, qrURL, err := b.quarkSvc.GetQRCode(userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"token": token, "qrUrl": qrURL})
}

// QuarkQRCodePoll POST /api/bot/quark/qrcode/poll
func (b *BotController) QuarkQRCodePoll(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == "" {
		rsp.Fail(c, "参数错误")
		return
	}
	st, err := b.quarkSvc.PollQRCode(userID, req.Token)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"status": st.Status})
}

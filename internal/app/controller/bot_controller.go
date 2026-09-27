package controller

import (
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
}

// NewBotController 创建机器人控制器
func NewBotController() *BotController {
	return &BotController{
		svc:      service.NewBotService(),
		musicSvc: service.NewMusicService(),
		quarkSvc: service.NewQuarkService(),
	}
}

// Status GET /api/bot/status
func (b *BotController) Status(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	status, err := b.svc.GetStatus(userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	// 以内存实际运行状态为准：数据库的 online 可能是进程重启前的陈旧状态
	if b.svc.IsRunning(userID) {
		status.Status = "online"
	} else if status.Status == "online" {
		status.Status = "offline"
	}
	rsp.OK(c, status)
}

// ResumeOnStartup 服务启动时恢复所有已登录机器人的消息轮询
func (b *BotController) ResumeOnStartup() {
	b.svc.ResumeAll()
}

// QRCode POST /api/bot/login/qrcode
func (b *BotController) QRCode(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	qr, err := b.svc.FetchQRCode(userID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, qr)
}

// ConfirmLogin POST /api/bot/login/confirm
func (b *BotController) ConfirmLogin(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	var req struct {
		QRCode           string `json:"qrcode"`
		QRCodeImgContent string `json:"qrcodeImgContent"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	qr := clawbot.QRCodeResult{QRCode: req.QRCode, QRCodeImgContent: req.QRCodeImgContent}
	creds, err := b.svc.WaitForLogin(userID, qr)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, creds)
}

// Start POST /api/bot/start
func (b *BotController) Start(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	if err := b.svc.Start(userID); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// Stop POST /api/bot/stop
func (b *BotController) Stop(c *gin.Context) {
	userID := middleware.CurrentUserID(c)
	if err := b.svc.Stop(userID); err != nil {
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

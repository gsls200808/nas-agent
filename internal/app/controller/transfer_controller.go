package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/middleware"
	"nas-agent/internal/app/service"
)

// TransferController 转存任务
type TransferController struct {
	svc *service.TransferService
}

// NewTransferController 构造
func NewTransferController(svc *service.TransferService) *TransferController {
	return &TransferController{svc: svc}
}

type transferReq struct {
	SourceURL      string `json:"sourceUrl"`      // 下载地址（http/https/ftp/m3u8）
	TargetServerID int64  `json:"targetServerId"` // 目标服务器
	TargetDir      string `json:"targetDir"`      // 目标存储目录
	LocalDir       string `json:"localDir"`       // 本地暂存目录，留空用默认临时目录
	TargetFileName string `json:"targetFileName"` // 用户指定的目标文件名（留空用源文件名）
}

// Create POST /api/transfer-tasks
func (t *TransferController) Create(c *gin.Context) {
	var req transferReq
	if err := c.ShouldBindJSON(&req); err != nil || req.SourceURL == "" {
		rsp.Fail(c, "参数错误")
		return
	}
	if req.TargetServerID == 0 {
		rsp.Fail(c, "请选择目标服务器")
		return
	}
	task, err := t.svc.Create(service.TransferRequest{
		UserID:         middleware.CurrentUserID(c),
		SourceURL:      req.SourceURL,
		TargetServerID: req.TargetServerID,
		TargetDir:      req.TargetDir,
		LocalDir:       req.LocalDir,
		TargetFileName: req.TargetFileName,
	})
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, task)
}

// RecentDirs GET /api/transfer-recent-dirs?serverId=1
// 当前登录用户在该目标服务器下近期使用的存储目录（最多 10 个，最近使用的在前）
func (t *TransferController) RecentDirs(c *gin.Context) {
	serverID, err := strconv.ParseInt(c.Query("serverId"), 10, 64)
	if err != nil || serverID <= 0 {
		rsp.Fail(c, "请选择目标服务器")
		return
	}
	list, err := t.svc.ListRecentDirs(middleware.CurrentUserID(c), serverID)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, list)
}

// List GET /api/transfer-tasks
func (t *TransferController) List(c *gin.Context) {
	rsp.OK(c, t.svc.List())
}

// Get GET /api/transfer-tasks/:taskId
func (t *TransferController) Get(c *gin.Context) {
	task, ok := t.svc.Get(c.Param("taskId"))
	if !ok {
		rsp.Fail(c, "任务不存在")
		return
	}
	rsp.OK(c, task)
}

type retryReq struct {
	Mode string `json:"mode"` // restart（从头开始）/ resume（从失败步骤开始）
}

// Retry POST /api/transfer-tasks/:taskId/retry
func (t *TransferController) Retry(c *gin.Context) {
	var req retryReq
	_ = c.ShouldBindJSON(&req)
	task, err := t.svc.Retry(c.Param("taskId"), req.Mode)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, task)
}

// Pause POST /api/transfer-tasks/:taskId/pause
// 暂停排队中/进行中的任务：中止传输，保留进度与本地暂存文件
func (t *TransferController) Pause(c *gin.Context) {
	task, err := t.svc.Pause(c.Param("taskId"))
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, task)
}

// Start POST /api/transfer-tasks/:taskId/start
// 开始（继续）已暂停的任务：从暂停时所处步骤续跑
func (t *TransferController) Start(c *gin.Context) {
	task, err := t.svc.Start(c.Param("taskId"))
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, task)
}

// Delete DELETE /api/transfer-tasks/:taskId
// 删除任务：运行中的先中止；本地暂存文件与任务记录一并清理
func (t *TransferController) Delete(c *gin.Context) {
	if err := t.svc.Delete(c.Param("taskId")); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

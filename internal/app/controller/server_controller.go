package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/service"
)

// ServerController 远端服务器 CRUD + 测试连接
type ServerController struct {
	svc *service.ServerService
}

// NewServerController 构造
func NewServerController(svc *service.ServerService) *ServerController {
	return &ServerController{svc: svc}
}

// List GET /api/servers
func (s *ServerController) List(c *gin.Context) {
	list, err := s.svc.List()
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, list)
}

// Create POST /api/servers
func (s *ServerController) Create(c *gin.Context) {
	var m model.RemoteServer
	if err := c.ShouldBindJSON(&m); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	id, err := s.svc.Create(&m)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"id": id})
}

// Update PUT /api/servers/:id
func (s *ServerController) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		rsp.Fail(c, "无效的 ID")
		return
	}
	var m model.RemoteServer
	if err := c.ShouldBindJSON(&m); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	m.ID = id
	if err := s.svc.Update(&m); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// Delete DELETE /api/servers/:id
func (s *ServerController) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		rsp.Fail(c, "无效的 ID")
		return
	}
	if err := s.svc.Delete(id); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// Test POST /api/servers/:id/test
func (s *ServerController) Test(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		rsp.Fail(c, "无效的 ID")
		return
	}
	if err := s.svc.TestConnection(id); err != nil {
		rsp.Fail(c, "连接失败: "+err.Error())
		return
	}
	rsp.OK(c, gin.H{"message": "连接成功"})
}

package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/service"
)

// UserController 用户管理（仅管理员）
type UserController struct {
	svc *service.UserService
}

// NewUserController 构造
func NewUserController(svc *service.UserService) *UserController {
	return &UserController{svc: svc}
}

// List GET /api/users
func (u *UserController) List(c *gin.Context) {
	list, err := u.svc.List()
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, list)
}

type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"isAdmin"`
}

// Create POST /api/users
func (u *UserController) Create(c *gin.Context) {
	var req createUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	id, err := u.svc.Create(req.Username, req.Password, req.IsAdmin)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"id": id})
}

// Delete DELETE /api/users/:id
func (u *UserController) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		rsp.Fail(c, "无效的 ID")
		return
	}
	if err := u.svc.Delete(id); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

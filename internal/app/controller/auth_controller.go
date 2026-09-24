package controller

import (
	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/service"
)

// AuthController 登录/登出/当前用户
type AuthController struct {
	auth *service.AuthService
}

// NewAuthController 构造
func NewAuthController(auth *service.AuthService) *AuthController {
	return &AuthController{auth: auth}
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login 登录
func (a *AuthController) Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.Fail(c, "参数错误")
		return
	}
	token, isAdmin, err := a.auth.Login(req.Username, req.Password)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"token": token, "username": req.Username, "isAdmin": isAdmin})
}

// Logout 登出
func (a *AuthController) Logout(c *gin.Context) {
	token, _ := c.Get("token")
	a.auth.Logout(token.(string))
	rsp.OK(c, nil)
}

// Profile 当前登录用户信息
func (a *AuthController) Profile(c *gin.Context) {
	username, _ := c.Get("username")
	isAdmin, _ := c.Get("isAdmin")
	rsp.OK(c, gin.H{"username": username, "isAdmin": isAdmin})
}

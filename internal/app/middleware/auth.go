package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/service"
)

// Auth 登录态校验：Authorization: Bearer <token> / X-Token / query token
func Auth(auth *service.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		username, userID, isAdmin, ok := auth.Validate(token)
		if !ok {
			rsp.FailWithCode(c, 401, "未登录或登录已过期")
			c.Abort()
			return
		}
		c.Set("token", token)
		c.Set("username", username)
		c.Set("userId", userID)
		c.Set("isAdmin", isAdmin)
		c.Next()
	}
}

// CurrentUserID 取当前登录用户 ID（未登录返回 0）
func CurrentUserID(c *gin.Context) int64 {
	if v, ok := c.Get("userId"); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

// RequireAdmin 要求管理员
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, _ := c.Get("isAdmin"); !v.(bool) {
			rsp.FailWithCode(c, 403, "需要管理员权限")
			c.Abort()
			return
		}
		c.Next()
	}
}

func extractToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if t := c.GetHeader("X-Token"); t != "" {
		return t
	}
	return c.Query("token") // 下载链接直连场景
}

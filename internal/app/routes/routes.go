package routes

import (
	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/controller"
	"nas-agent/internal/app/middleware"
	"nas-agent/internal/app/service"
)

// Register 注册全部业务路由。
// 所有后端接口统一挂在 /api 前缀下，根路径 / 留给前端静态页面（SPA）。
// defaultDownloadDir 为下载转存任务的默认本地暂存目录。
func Register(r *gin.Engine, auth *service.AuthService, defaultDownloadDir string) {
	authCtrl := controller.NewAuthController(auth)
	serverCtrl := controller.NewServerController(service.NewServerService())
	fileCtrl := controller.NewFileController(service.NewFileService())
	transferCtrl := controller.NewTransferController(service.NewTransferService(defaultDownloadDir))
	userCtrl := controller.NewUserController(service.NewUserService())

	api := r.Group("/api")
	{
		api.POST("/auth/login", authCtrl.Login)

		authed := api.Group("")
		authed.Use(middleware.Auth(auth))
		{
			authed.POST("/auth/logout", authCtrl.Logout)
			authed.GET("/auth/profile", authCtrl.Profile)

			// 远端服务器管理
			authed.GET("/servers", serverCtrl.List)
			authed.POST("/servers", serverCtrl.Create)
			authed.PUT("/servers/:id", serverCtrl.Update)
			authed.DELETE("/servers/:id", serverCtrl.Delete)
			authed.POST("/servers/:id/test", serverCtrl.Test)

			// 远端文件操作
			authed.GET("/servers/:id/files", fileCtrl.List)
			authed.GET("/servers/:id/files/stat", fileCtrl.Stat)
			authed.GET("/servers/:id/files/download", fileCtrl.Download)
			authed.POST("/servers/:id/files/upload", fileCtrl.Upload)
			authed.POST("/servers/:id/files/mkdir", fileCtrl.Mkdir)
			authed.POST("/servers/:id/files/rename", fileCtrl.Rename)
			authed.DELETE("/servers/:id/files", fileCtrl.Delete)

			// 转存任务：下载地址（http/ftp/m3u8）→ 本地暂存目录 → 转存到目标服务器的存储目录
			authed.POST("/transfer-tasks", transferCtrl.Create)
			authed.GET("/transfer-tasks", transferCtrl.List)
			authed.GET("/transfer-tasks/:taskId", transferCtrl.Get)
			authed.POST("/transfer-tasks/:taskId/retry", transferCtrl.Retry)

			// 当前用户在某目标服务器下近期使用的存储目录
			authed.GET("/transfer-recent-dirs", transferCtrl.RecentDirs)

			// 用户管理（仅管理员）
			admin := authed.Group("/users")
			admin.Use(middleware.RequireAdmin())
			{
				admin.GET("", userCtrl.List)
				admin.POST("", userCtrl.Create)
				admin.DELETE("/:id", userCtrl.Delete)
			}
		}
	}
}

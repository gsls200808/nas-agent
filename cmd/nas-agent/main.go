package main

import (
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/config"
	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/dao"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/routes"
	"nas-agent/internal/app/service"
	"nas-agent/web"
)

// webFS 为内嵌的前端静态资源
var webFS = web.Dist

func main() {
	cfgPath := "configs/application.yaml"
	if p := os.Getenv("NAS_AGENT_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		panic(fmt.Sprintf("加载配置失败: %v", err))
	}
	if err := util.InitLogger(cfg.Log.Level); err != nil {
		panic(fmt.Sprintf("初始化日志失败: %v", err))
	}
	defer util.Sync()
	logger := util.Logger

	if err := dao.InitSQLite(cfg.Storage.Database); err != nil {
		logger.Fatalf("初始化数据库失败: %v", err)
	}
	defer dao.Close()
	seedAdmin(cfg.Admin.Username, cfg.Admin.Password)

	if cfg.Server.Mode == "debug" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	auth := service.NewAuthService()
	routes.Register(r, auth, cfg.Storage.DownloadDir)

	// ---- 前端静态资源（/ 留给前端）+ 手写 NoRoute SPA 兜底 ----
	dist, err := fs.Sub(webFS, "dist")
	if err != nil {
		logger.Fatalf("读取内嵌前端资源失败: %v", err)
	}
	// NoRoute：/api 前缀返回 JSON 404；能命中静态文件则返回文件；否则一律回退 index.html（支持前端 history 路由）
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "msg": "接口不存在"})
			return
		}
		p := strings.TrimPrefix(path.Clean("/"+c.Request.URL.Path), "/")
		if p == "" || p == "." {
			p = "index.html"
		}
		if data, err := fs.ReadFile(dist, p); err == nil {
			ct := mime.TypeByExtension(filepath.Ext(p))
			if ct == "" {
				ct = "application/octet-stream"
			}
			c.Data(http.StatusOK, ct, data)
			return
		}
		data, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			c.String(http.StatusNotFound, "前端资源未构建：请先在 web/ 目录执行 npm run build")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	logger.Infof("NAS Agent 启动于 http://localhost%s （默认账号 %s）", addr, cfg.Admin.Username)
	if err := r.Run(addr); err != nil {
		logger.Fatalf("服务启动失败: %v", err)
	}
}

// seedAdmin 首次启动时按配置播种管理员账号
func seedAdmin(username, password string) {
	n, err := dao.CountUsers()
	if err != nil || n > 0 {
		return
	}
	if _, err := dao.CreateUser(&model.User{
		Username:     username,
		PasswordHash: dao.HashPassword(password),
		IsAdmin:      true,
	}); err != nil {
		panic(fmt.Sprintf("播种管理员账号失败: %v", err))
	}
}

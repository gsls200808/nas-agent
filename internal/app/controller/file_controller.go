package controller

import (
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"nas-agent/internal/app/common/rsp"
	"nas-agent/internal/app/service"
)

// FileController 远端文件浏览与传输
type FileController struct {
	svc *service.FileService
}

// NewFileController 构造
func NewFileController(svc *service.FileService) *FileController {
	return &FileController{svc: svc}
}

func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		rsp.Fail(c, "无效的服务器 ID")
		return 0, false
	}
	return id, true
}

// List GET /api/servers/:id/files?path=/
func (f *FileController) List(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dir := c.DefaultQuery("path", "/")
	list, err := f.svc.List(id, dir)
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, list)
}

// Stat GET /api/servers/:id/files/stat?path=
func (f *FileController) Stat(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	fi, err := f.svc.Stat(id, c.Query("path"))
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, fi)
}

// Download GET /api/servers/:id/files/download?path=
func (f *FileController) Download(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	rc, fi, err := f.svc.Download(id, c.Query("path"))
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	defer rc.Close()
	// RFC 5987 编码文件名，兼容中文
	encoded := url.PathEscape(fi.Name)
	c.Header("Content-Disposition", `attachment; filename="download"; filename*=UTF-8''`+encoded)
	c.Header("Content-Type", "application/octet-stream")
	if fi.Size > 0 {
		c.Header("Content-Length", strconv.FormatInt(fi.Size, 10))
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, rc)
}

// Upload POST /api/servers/:id/files/upload?path=/dir  multipart: file
func (f *FileController) Upload(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dir := c.DefaultQuery("path", "/")
	fileHeader, err := c.FormFile("file")
	if err != nil {
		rsp.Fail(c, "请选择上传文件")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	defer src.Close()
	if err := f.svc.Upload(id, dir, fileHeader.Filename, src, fileHeader.Size); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, gin.H{"name": fileHeader.Filename, "size": fileHeader.Size})
}

type pathReq struct {
	Path string `json:"path"`
}

// Mkdir POST /api/servers/:id/files/mkdir
func (f *FileController) Mkdir(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req pathReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Path == "" {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := f.svc.Mkdir(id, req.Path); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

// Delete DELETE /api/servers/:id/files?path=
func (f *FileController) Delete(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := f.svc.Remove(id, c.Query("path")); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

type renameReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Rename POST /api/servers/:id/files/rename
func (f *FileController) Rename(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req renameReq
	if err := c.ShouldBindJSON(&req); err != nil || req.From == "" || req.To == "" {
		rsp.Fail(c, "参数错误")
		return
	}
	if err := f.svc.Rename(id, req.From, req.To); err != nil {
		rsp.Fail(c, err.Error())
		return
	}
	rsp.OK(c, nil)
}

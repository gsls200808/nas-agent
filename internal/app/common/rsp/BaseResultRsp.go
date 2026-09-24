package rsp

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BaseResult 统一响应结构
type BaseResult struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data,omitempty"`
}

// OK 成功响应
func OK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, BaseResult{Code: 200, Msg: "success", Data: data})
}

// Fail 失败响应
func Fail(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, BaseResult{Code: 500, Msg: msg})
}

// FailWithCode 指定 HTTP 状态码的失败响应
func FailWithCode(c *gin.Context, httpCode int, msg string) {
	c.JSON(httpCode, BaseResult{Code: httpCode, Msg: msg})
}

// PageResult 分页响应
type PageResult struct {
	Code  int         `json:"code"`
	Msg   string      `json:"msg"`
	Total int64       `json:"total"`
	Data  interface{} `json:"data"`
}

// OKPage 分页成功响应
func OKPage(c *gin.Context, total int64, data interface{}) {
	c.JSON(http.StatusOK, PageResult{Code: 200, Msg: "success", Total: total, Data: data})
}

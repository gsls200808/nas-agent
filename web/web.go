// Package web 内嵌前端构建产物（web/dist），供 main 以 go:embed 打包进单文件可执行程序。
package web

import "embed"

//go:embed dist
var Dist embed.FS

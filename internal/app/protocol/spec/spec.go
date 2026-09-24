// Package spec 定义协议客户端的统一接口与连接参数。
// 独立成包以避免 protocol（工厂）与各协议实现子包之间的循环引用：
// 工厂包和 ftp/smb/nfs/webdav 实现包都只依赖本包。
package spec

import (
	"io"

	"nas-agent/internal/app/model"
)

// Config 构造协议客户端所需的连接参数
type Config struct {
	Host      string // 远端主机
	Port      int    // 远端端口
	Username  string // 用户名（NFS 可留空）
	Password  string // 密码（NFS 可留空）
	Share     string // SMB 共享名 / NFS 导出路径 / WebDAV 基础路径 / FTP 起始目录
	Workgroup string // SMB 工作组/域（默认 WORKGROUP）
}

// Client 协议文件客户端统一接口
type Client interface {
	// Open 建立连接并完成认证/挂载
	Open() error
	// Close 关闭连接
	Close() error
	// List 列出目录内容
	List(dir string) ([]model.FileInfo, error)
	// Stat 查询文件/目录信息
	Stat(path string) (*model.FileInfo, error)
	// Download 下载文件，调用方负责 Close 返回的 ReadCloser
	Download(path string) (io.ReadCloser, error)
	// Upload 上传文件（size 为总大小，未知时传 -1）
	Upload(path string, r io.Reader, size int64) error
	// Mkdir 创建目录
	Mkdir(path string) error
	// Remove 删除文件或目录
	Remove(path string) error
	// Rename 重命名/移动
	Rename(from, to string) error
}

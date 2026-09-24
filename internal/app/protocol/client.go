// Package protocol 定义 NAS Agent 连接远端文件服务器的统一客户端接口。
// FTP / SMB / NFS / WebDAV 四种协议各自实现该接口，且均使用 Go 标准库手写，不引入第三方协议库。
package protocol

import (
	"fmt"

	"nas-agent/internal/app/common/consts"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/ftp"
	"nas-agent/internal/app/protocol/nfs"
	"nas-agent/internal/app/protocol/smb"
	"nas-agent/internal/app/protocol/spec"
	"nas-agent/internal/app/protocol/webdav"
)

// Config 构造协议客户端所需的连接参数（spec.Config 的别名）
type Config = spec.Config

// Client 协议文件客户端统一接口（spec.Client 的别名）
type Client = spec.Client

// New 根据远端服务器配置创建协议客户端（尚未连接）
func New(s model.RemoteServer) (Client, error) {
	cfg := Config{
		Host:      s.Host,
		Port:      s.Port,
		Username:  s.Username,
		Password:  s.Password,
		Share:     s.Share,
		Workgroup: s.Workgroup,
	}
	switch s.Protocol {
	case consts.ProtoFTP:
		return ftp.New(cfg), nil
	case consts.ProtoSMB:
		return smb.New(cfg), nil
	case consts.ProtoNFS:
		return nfs.New(cfg), nil
	case consts.ProtoWebDAV:
		return webdav.New(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", s.Protocol)
	}
}

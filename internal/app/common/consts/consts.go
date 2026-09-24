package consts

// 文件服务协议（Agent 作为客户端连接）
const (
	ProtoFTP    = "ftp"
	ProtoSMB    = "smb"
	ProtoNFS    = "nfs"
	ProtoWebDAV = "webdav"
)

// 各协议默认端口
const (
	DefaultFTPPort    = 21
	DefaultSMBPort    = 445
	DefaultNFSPort    = 2049
	DefaultWebDAVPort = 80
)

// ProtoNames 协议显示名称
var ProtoNames = map[string]string{
	ProtoFTP:    "FTP",
	ProtoSMB:    "SMB/CIFS",
	ProtoNFS:    "NFS",
	ProtoWebDAV: "WebDAV",
}

// ValidProtocol 判断协议是否支持
func ValidProtocol(p string) bool {
	switch p {
	case ProtoFTP, ProtoSMB, ProtoNFS, ProtoWebDAV:
		return true
	}
	return false
}

package model

import "time"

// RemoteServer 远端文件服务器（Agent 作为客户端连接）
type RemoteServer struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`               // 备注名称
	Protocol  string    `json:"protocol"`           // ftp / smb / nfs / webdav
	Host      string    `json:"host"`               // 远端主机
	Port      int       `json:"port"`               // 远端端口
	Username  string    `json:"username"`           // 用户名（NFS 可留空）
	Password  string    `json:"password,omitempty"` // 密码（NFS 可留空）
	Share     string    `json:"share"`              // SMB 共享名 / NFS 导出路径 / WebDAV 基础路径 / FTP 起始目录
	Workgroup string    `json:"workgroup"`          // SMB 工作组/域（默认 WORKGROUP）
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// User Web 管理后台用户
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	IsAdmin      bool      `json:"isAdmin"`
	CreatedAt    time.Time `json:"createdAt"`
}

// UserVO 返回前端的用户视图
type UserVO struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"isAdmin"`
	CreatedAt time.Time `json:"createdAt"`
}

// ToVO 转换
func (u *User) ToVO() UserVO {
	return UserVO{ID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
}

// FileInfo 统一的文件条目（各协议客户端转换为此结构）
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// TransferTask 转存任务：从下载地址下载到本地暂存目录 → 转存到目标服务器的存储目录
type TransferTask struct {
	ID               string     `json:"id"`                       // 任务 ID
	SourceURL        string     `json:"sourceUrl"`                // 下载地址（http/https/ftp/m3u8）
	SourceType       string     `json:"sourceType"`               // http / ftp / m3u8
	TargetServerID   int64      `json:"targetServerId"`           // 目标服务器
	TargetServerName string     `json:"targetServerName"`         // 目标服务器名称
	TargetDir        string     `json:"targetDir"`                // 目标存储目录
	TargetPath       string     `json:"targetPath"`               // 转存后的完整路径
	TargetFileName   string     `json:"targetFileName,omitempty"` // 用户指定的目标文件名（留空用源文件名）
	LocalDir         string     `json:"localDir"`                 // 本地暂存目录
	LocalPath        string     `json:"localPath"`                // 本地暂存文件路径（失败时保留）
	FileName         string     `json:"fileName"`                 // 文件名
	Size             int64      `json:"size"`                     // 文件大小（未知为 -1）
	Downloaded       int64      `json:"downloaded"`               // 已下载字节数
	Uploaded         int64      `json:"uploaded"`                 // 已转存字节数
	Segments         int        `json:"segments"`                 // m3u8 总分片数
	DownloadedSegs   int        `json:"downloadedSegs"`           // m3u8 已下载分片数
	Status           string     `json:"status"`                   // pending / running / paused / success / failed
	Phase            string     `json:"phase"`                    // pending / downloading / merging / transferring / done
	Progress         int        `json:"progress"`                 // 0-100
	FailedStep       string     `json:"failedStep,omitempty"`     // 失败时所处步骤：download / merge / upload
	Error            string     `json:"error,omitempty"`          // 失败原因
	CreatedAt        time.Time  `json:"createdAt"`
	FinishedAt       *time.Time `json:"finishedAt,omitempty"`

	// ResumeStep 重试时从该步骤开始（空 = 从头开始），仅内部使用
	ResumeStep string `json:"-"`
}

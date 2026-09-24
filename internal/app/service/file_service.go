package service

import (
	"fmt"
	"io"
	"path"

	"nas-agent/internal/app/dao"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol"
	"nas-agent/internal/app/protocol/spec"
)

// FileService 远端文件操作：每次操作新建连接，用完即关（不做常驻连接池）
type FileService struct{}

// NewFileService 创建文件服务
func NewFileService() *FileService { return &FileService{} }

// openClient 取服务器配置并建立协议客户端
func (FileService) openClient(serverID int64) (spec.Client, *model.RemoteServer, error) {
	return openServerClient(serverID)
}

// openServerClient 按服务器 ID 建立已连接的协议客户端
func openServerClient(serverID int64) (spec.Client, *model.RemoteServer, error) {
	srv, err := dao.GetServer(serverID)
	if err != nil {
		return nil, nil, fmt.Errorf("服务器不存在: %d", serverID)
	}
	cli, err := protocol.New(*srv)
	if err != nil {
		return nil, nil, err
	}
	if err := cli.Open(); err != nil {
		return nil, nil, err
	}
	return cli, srv, nil
}

// streamCloser 下载流关闭时同时关闭协议客户端连接
type streamCloser struct {
	rc  io.ReadCloser
	cli spec.Client
}

func (s *streamCloser) Read(p []byte) (int, error) { return s.rc.Read(p) }

func (s *streamCloser) Close() error {
	readErr := s.rc.Close()
	cliErr := s.cli.Close()
	if readErr != nil {
		return readErr
	}
	return cliErr
}

// List 列目录
func (f FileService) List(serverID int64, dir string) ([]model.FileInfo, error) {
	cli, _, err := f.openClient(serverID)
	if err != nil {
		return nil, err
	}
	list, err := cli.List(dir)
	_ = cli.Close()
	return list, err
}

// Stat 查询条目
func (f FileService) Stat(serverID int64, p string) (*model.FileInfo, error) {
	cli, _, err := f.openClient(serverID)
	if err != nil {
		return nil, err
	}
	fi, err := cli.Stat(p)
	_ = cli.Close()
	return fi, err
}

// Download 打开下载流，调用方必须 Close（会连带关闭底层协议连接）
func (f FileService) Download(serverID int64, p string) (io.ReadCloser, *model.FileInfo, error) {
	cli, _, err := f.openClient(serverID)
	if err != nil {
		return nil, nil, err
	}
	fi, err := cli.Stat(p)
	if err != nil {
		_ = cli.Close()
		return nil, nil, err
	}
	if fi.IsDir {
		_ = cli.Close()
		return nil, nil, fmt.Errorf("不能下载目录: %s", p)
	}
	rc, err := cli.Download(p)
	if err != nil {
		_ = cli.Close()
		return nil, nil, err
	}
	return &streamCloser{rc: rc, cli: cli}, fi, nil
}

// Upload 上传文件到 dir 目录，文件名为 filename
func (f FileService) Upload(serverID int64, dir, filename string, r io.Reader, size int64) error {
	if filename == "" {
		return fmt.Errorf("文件名不能为空")
	}
	cli, _, err := f.openClient(serverID)
	if err != nil {
		return err
	}
	target := path.Join(dir, filename)
	err = cli.Upload(target, r, size)
	if cerr := cli.Close(); err == nil {
		err = cerr
	}
	return err
}

// Mkdir 新建目录
func (f FileService) Mkdir(serverID int64, p string) error {
	return f.run(serverID, func(cli spec.Client) error { return cli.Mkdir(p) })
}

// Remove 删除文件/目录
func (f FileService) Remove(serverID int64, p string) error {
	return f.run(serverID, func(cli spec.Client) error { return cli.Remove(p) })
}

// Rename 重命名/移动
func (f FileService) Rename(serverID int64, from, to string) error {
	return f.run(serverID, func(cli spec.Client) error { return cli.Rename(from, to) })
}

// run 执行单次操作并关闭连接
func (f FileService) run(serverID int64, fn func(cli spec.Client) error) error {
	cli, _, err := f.openClient(serverID)
	if err != nil {
		return err
	}
	err = fn(cli)
	if cerr := cli.Close(); err == nil {
		err = cerr
	}
	return err
}

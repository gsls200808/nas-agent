package service

import (
	"errors"
	"fmt"

	"nas-agent/internal/app/common/consts"
	"nas-agent/internal/app/dao"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol"
)

// ServerService 远端服务器管理
type ServerService struct{}

// NewServerService 创建服务
func NewServerService() *ServerService { return &ServerService{} }

// normalize 校验并补全默认值
func (ServerService) normalize(s *model.RemoteServer) error {
	if s.Name == "" {
		return errors.New("名称不能为空")
	}
	if s.Host == "" {
		return errors.New("主机地址不能为空")
	}
	if !consts.ValidProtocol(s.Protocol) {
		return fmt.Errorf("不支持的协议: %s", s.Protocol)
	}
	if s.Port == 0 {
		switch s.Protocol {
		case consts.ProtoFTP:
			s.Port = consts.DefaultFTPPort
		case consts.ProtoSMB:
			s.Port = consts.DefaultSMBPort
		case consts.ProtoNFS:
			s.Port = consts.DefaultNFSPort
		case consts.ProtoWebDAV:
			s.Port = consts.DefaultWebDAVPort
		}
	}
	if s.Protocol == consts.ProtoSMB && s.Workgroup == "" {
		s.Workgroup = "WORKGROUP"
	}
	return nil
}

// List 全部服务器
func (ServerService) List() ([]model.RemoteServer, error) {
	return dao.ListServers()
}

// Get 查询
func (ServerService) Get(id int64) (*model.RemoteServer, error) {
	return dao.GetServer(id)
}

// Create 新建
func (s ServerService) Create(m *model.RemoteServer) (int64, error) {
	if err := s.normalize(m); err != nil {
		return 0, err
	}
	return dao.CreateServer(m)
}

// Update 更新
func (s ServerService) Update(m *model.RemoteServer) error {
	if err := s.normalize(m); err != nil {
		return err
	}
	return dao.UpdateServer(m)
}

// Delete 删除
func (ServerService) Delete(id int64) error {
	return dao.DeleteServer(id)
}

// TestConnection 实际尝试连接（新建客户端 -> Open -> Close）
func (ServerService) TestConnection(id int64) error {
	m, err := dao.GetServer(id)
	if err != nil {
		return err
	}
	cli, err := protocol.New(*m)
	if err != nil {
		return err
	}
	if err := cli.Open(); err != nil {
		return err
	}
	return cli.Close()
}

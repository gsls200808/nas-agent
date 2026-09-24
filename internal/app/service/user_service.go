package service

import (
	"errors"

	"nas-agent/internal/app/dao"
	"nas-agent/internal/app/model"
)

// UserService 用户管理
type UserService struct{}

// NewUserService 创建用户服务
func NewUserService() *UserService { return &UserService{} }

// List 用户列表
func (UserService) List() ([]model.UserVO, error) {
	list, err := dao.ListUsers()
	if err != nil {
		return nil, err
	}
	out := make([]model.UserVO, 0, len(list))
	for _, u := range list {
		out = append(out, u.ToVO())
	}
	return out, nil
}

// Create 创建用户
func (UserService) Create(username, password string, isAdmin bool) (int64, error) {
	if username == "" || password == "" {
		return 0, errors.New("用户名和密码不能为空")
	}
	if _, err := dao.GetUserByUsername(username); err == nil {
		return 0, errors.New("用户名已存在")
	}
	u := &model.User{
		Username:     username,
		PasswordHash: dao.HashPassword(password),
		IsAdmin:      isAdmin,
	}
	return dao.CreateUser(u)
}

// Delete 删除用户；禁止删除最后一个管理员
func (UserService) Delete(id int64) error {
	u, err := dao.GetUserByID(id)
	if err != nil {
		return err
	}
	if u.IsAdmin {
		n, err := dao.CountAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return errors.New("至少保留一个管理员账号")
		}
	}
	return dao.DeleteUserByID(id)
}

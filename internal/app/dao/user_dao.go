package dao

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"nas-agent/internal/app/model"
)

// HashPassword SHA256 哈希密码
func HashPassword(pwd string) string {
	sum := sha256.Sum256([]byte(pwd))
	return hex.EncodeToString(sum[:])
}

// GetUserByUsername 按用户名查询
func GetUserByUsername(username string) (*model.User, error) {
	row := DB.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM user WHERE username=?`, username)
	var u model.User
	var isAdmin int
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

// CountUsers 用户数量
func CountUsers() (int64, error) {
	var n int64
	err := DB.QueryRow(`SELECT COUNT(1) FROM user`).Scan(&n)
	return n, err
}

// CountAdmins 管理员数量
func CountAdmins() (int64, error) {
	var n int64
	err := DB.QueryRow(`SELECT COUNT(1) FROM user WHERE is_admin=1`).Scan(&n)
	return n, err
}

// GetUserByID 按 ID 查询
func GetUserByID(id int64) (*model.User, error) {
	row := DB.QueryRow(`SELECT id, username, password_hash, is_admin, created_at FROM user WHERE id=?`, id)
	var u model.User
	var isAdmin int
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	return &u, nil
}

// DeleteUserByID 按 ID 删除用户
func DeleteUserByID(id int64) error {
	res, err := DB.Exec(`DELETE FROM user WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateUser 创建用户
func CreateUser(u *model.User) (int64, error) {
	res, err := DB.Exec(`INSERT INTO user (username, password_hash, is_admin) VALUES (?,?,?)`,
		u.Username, u.PasswordHash, boolToInt(u.IsAdmin))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListUsers 用户列表
func ListUsers() ([]model.User, error) {
	rows, err := DB.Query(`SELECT id, username, password_hash, is_admin, created_at FROM user ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []model.User
	for rows.Next() {
		var u model.User
		var isAdmin int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &isAdmin, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		list = append(list, u)
	}
	return list, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

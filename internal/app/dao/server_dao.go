package dao

import (
	"database/sql"
	"errors"
	"time"

	"nas-agent/internal/app/model"
)

// ErrNotFound 记录不存在
var ErrNotFound = errors.New("record not found")

// ListServers 查询全部远端服务器
func ListServers() ([]model.RemoteServer, error) {
	rows, err := DB.Query(`SELECT id, name, protocol, host, port, username, password, share, workgroup,
		created_at, updated_at FROM remote_server ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []model.RemoteServer
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// GetServer 按 ID 查询
func GetServer(id int64) (*model.RemoteServer, error) {
	row := DB.QueryRow(`SELECT id, name, protocol, host, port, username, password, share, workgroup,
		created_at, updated_at FROM remote_server WHERE id = ?`, id)
	s, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateServer 新建
func CreateServer(s *model.RemoteServer) (int64, error) {
	res, err := DB.Exec(`INSERT INTO remote_server
		(name, protocol, host, port, username, password, share, workgroup)
		VALUES (?,?,?,?,?,?,?,?)`,
		s.Name, s.Protocol, s.Host, s.Port, s.Username, s.Password, s.Share, s.Workgroup)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateServer 更新
func UpdateServer(s *model.RemoteServer) error {
	res, err := DB.Exec(`UPDATE remote_server SET
		name=?, protocol=?, host=?, port=?, username=?, password=?, share=?, workgroup=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		s.Name, s.Protocol, s.Host, s.Port, s.Username, s.Password, s.Share, s.Workgroup, s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteServer 删除
func DeleteServer(id int64) error {
	res, err := DB.Exec(`DELETE FROM remote_server WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanServer(r rowScanner) (model.RemoteServer, error) {
	var s model.RemoteServer
	var createdAt, updatedAt time.Time
	err := r.Scan(&s.ID, &s.Name, &s.Protocol, &s.Host, &s.Port, &s.Username, &s.Password,
		&s.Share, &s.Workgroup, &createdAt, &updatedAt)
	if err != nil {
		return s, err
	}
	s.CreatedAt = createdAt
	s.UpdatedAt = updatedAt
	return s, nil
}

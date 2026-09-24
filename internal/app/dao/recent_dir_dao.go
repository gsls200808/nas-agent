package dao

// maxRecentDir 每个用户在每个目标服务器下保留的近期目录条数
const maxRecentDir = 10

// SaveRecentDir 记录一次目录使用（与用户、目标服务器关联）：
// 同一用户 + 服务器 + 目录只保留最新一条，并裁剪到最近 maxRecentDir 条，多余的删除
func SaveRecentDir(userID, serverID int64, dir string) error {
	if DB == nil || userID <= 0 || serverID <= 0 || dir == "" {
		return nil
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 已存在则删掉旧记录，重新插入，保证 id 递增即"最近使用"的顺序
	if _, err := tx.Exec(`DELETE FROM transfer_recent_dir WHERE user_id=? AND server_id=? AND dir=?`,
		userID, serverID, dir); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO transfer_recent_dir (user_id, server_id, dir) VALUES (?,?,?)`,
		userID, serverID, dir); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM transfer_recent_dir
		WHERE user_id=? AND server_id=? AND id NOT IN (
			SELECT id FROM transfer_recent_dir WHERE user_id=? AND server_id=? ORDER BY id DESC LIMIT ?
		)`, userID, serverID, userID, serverID, maxRecentDir); err != nil {
		return err
	}
	return tx.Commit()
}

// ListRecentDirs 查询某用户在某目标服务器下近期使用的目录（最近使用的在前）
func ListRecentDirs(userID, serverID int64) ([]string, error) {
	if DB == nil || userID <= 0 || serverID <= 0 {
		return nil, nil
	}
	rows, err := DB.Query(`SELECT dir FROM transfer_recent_dir
		WHERE user_id=? AND server_id=? ORDER BY id DESC LIMIT ?`, userID, serverID, maxRecentDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		list = append(list, dir)
	}
	return list, rows.Err()
}

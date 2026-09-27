package dao

import (
	"database/sql"
	"errors"
	"time"

	"nas-agent/internal/app/model"
)

const transferTaskCols = `id, source_url, source_type, target_server_id, target_server_name,
	target_dir, target_path, target_file_name, local_dir, local_path, file_name,
	size, downloaded, uploaded, segments, downloaded_segs,
	status, phase, progress, failed_step, error, created_at, finished_at`

// SaveTransferTask 插入或按 ID 覆盖转存任务
func SaveTransferTask(t *model.TransferTask) error {
	if DB == nil {
		return nil
	}
	var finishedAt interface{}
	if t.FinishedAt != nil {
		finishedAt = *t.FinishedAt
	}
	_, err := DB.Exec(`INSERT OR REPLACE INTO transfer_task
		(`+transferTaskCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SourceURL, t.SourceType, t.TargetServerID, t.TargetServerName,
		t.TargetDir, t.TargetPath, t.TargetFileName, t.LocalDir, t.LocalPath, t.FileName,
		t.Size, t.Downloaded, t.Uploaded, t.Segments, t.DownloadedSegs,
		t.Status, t.Phase, t.Progress, t.FailedStep, t.Error, t.CreatedAt, finishedAt)
	return err
}

// GetTransferTask 按 ID 查询转存任务
func GetTransferTask(id string) (*model.TransferTask, error) {
	if DB == nil {
		return nil, ErrNotFound
	}
	row := DB.QueryRow(`SELECT `+transferTaskCols+` FROM transfer_task WHERE id = ?`, id)
	t, err := scanTransferTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListTransferTasks 按创建时间倒序查询转存任务
func ListTransferTasks(limit int) ([]*model.TransferTask, error) {
	if DB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := DB.Query(`SELECT `+transferTaskCols+` FROM transfer_task
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.TransferTask
	for rows.Next() {
		t, err := scanTransferTask(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// InterruptUnfinishedTasks 把上次进程退出时仍在进行中的任务标记为失败（重启后无法继续执行）。
// 已暂停的任务不受影响，重启后仍可“开始”继续
func InterruptUnfinishedTasks(msg string) error {
	if DB == nil {
		return nil
	}
	_, err := DB.Exec(`UPDATE transfer_task
		SET status='failed', error=?, finished_at=?
		WHERE status IN ('pending','running')`, msg, time.Now())
	return err
}

// DeleteTransferTask 按 ID 删除转存任务
func DeleteTransferTask(id string) error {
	if DB == nil {
		return nil
	}
	_, err := DB.Exec(`DELETE FROM transfer_task WHERE id = ?`, id)
	return err
}

func scanTransferTask(r rowScanner) (*model.TransferTask, error) {
	var (
		t          model.TransferTask
		finishedAt sql.NullTime
	)
	err := r.Scan(&t.ID, &t.SourceURL, &t.SourceType, &t.TargetServerID, &t.TargetServerName,
		&t.TargetDir, &t.TargetPath, &t.TargetFileName, &t.LocalDir, &t.LocalPath, &t.FileName,
		&t.Size, &t.Downloaded, &t.Uploaded, &t.Segments, &t.DownloadedSegs,
		&t.Status, &t.Phase, &t.Progress, &t.FailedStep, &t.Error, &t.CreatedAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		t.FinishedAt = &finishedAt.Time
	}
	return &t, nil
}

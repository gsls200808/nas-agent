package dao

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB 全局 SQLite 实例
var DB *sql.DB

// InitSQLite 初始化 SQLite 数据库并建表
func InitSQLite(dbPath string) error {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	DB = db
	return migrate()
}

// migrate 建表
func migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS remote_server (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '',
			share TEXT NOT NULL DEFAULT '',
			workgroup TEXT NOT NULL DEFAULT 'WORKGROUP',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS user (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS transfer_task (
			id TEXT PRIMARY KEY,
			source_url TEXT NOT NULL DEFAULT '',
			source_type TEXT NOT NULL DEFAULT '',
			target_server_id INTEGER NOT NULL DEFAULT 0,
			target_server_name TEXT NOT NULL DEFAULT '',
			target_dir TEXT NOT NULL DEFAULT '',
			target_path TEXT NOT NULL DEFAULT '',
			target_file_name TEXT NOT NULL DEFAULT '',
			local_dir TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL DEFAULT '',
			file_name TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT -1,
			downloaded INTEGER NOT NULL DEFAULT 0,
			uploaded INTEGER NOT NULL DEFAULT 0,
			segments INTEGER NOT NULL DEFAULT 0,
			downloaded_segs INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			phase TEXT NOT NULL DEFAULT '',
			progress INTEGER NOT NULL DEFAULT 0,
			failed_step TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at DATETIME,
			finished_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_transfer_task_created_at ON transfer_task(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS transfer_recent_dir (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			server_id INTEGER NOT NULL,
			dir TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_transfer_recent_dir ON transfer_recent_dir(user_id, server_id, id DESC)`,
	}
	for _, s := range stmts {
		if _, err := DB.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// Close 关闭数据库
func Close() {
	if DB != nil {
		_ = DB.Close()
	}
}

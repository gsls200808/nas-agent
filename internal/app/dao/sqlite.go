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
		`CREATE TABLE IF NOT EXISTS music_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			server_id INTEGER NOT NULL DEFAULT 0,
			music_dir TEXT NOT NULL DEFAULT '/',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_music_config_user ON music_config(user_id)`,
		`CREATE TABLE IF NOT EXISTS music_play_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			chat_id TEXT NOT NULL DEFAULT '',
			song_name TEXT NOT NULL DEFAULT '',
			song_path TEXT NOT NULL DEFAULT '',
			server_id INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'success',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_music_play_log_user ON music_play_log(user_id, id DESC)`,
		`CREATE TABLE IF NOT EXISTS bot_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			bot_token TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			bot_id TEXT NOT NULL DEFAULT '',
			ilink_user_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'offline',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_bot_config_user ON bot_config(user_id)`,
		`CREATE TABLE IF NOT EXISTS bot_account (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			bot_token TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			bot_id TEXT NOT NULL DEFAULT '',
			ilink_user_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'offline',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_bot_account_bot ON bot_account(bot_id) WHERE bot_id != ''`,
		// 一次性迁移：旧 bot_config（单微信模型）导入 bot_account
		`INSERT INTO bot_account(user_id, name, bot_token, base_url, bot_id, ilink_user_id, status)
		 SELECT user_id, '微信', bot_token, base_url, bot_id, ilink_user_id, status FROM bot_config
		 WHERE bot_token != '' AND NOT EXISTS(SELECT 1 FROM bot_account)`,
		// QQ 机器人渠道（bot_account 扩展列，兼容已有库）
		`CREATE TABLE IF NOT EXISTS quark_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			cookie TEXT NOT NULL DEFAULT '',
			nickname TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'unbound',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_quark_config_user ON quark_config(user_id)`,
		`CREATE TABLE IF NOT EXISTS video_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 0,
			server_id INTEGER NOT NULL DEFAULT 0,
			video_dir TEXT NOT NULL DEFAULT '/',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_video_config_user ON video_config(user_id)`,
	}
	for _, s := range stmts {
		if _, err := DB.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// video_config 拆分为电视/电影两套目录（幂等：仅当列不存在时添加）
	for _, c := range []struct {
		name string
		ddl  string
	}{
		{"tv_server_id", "ALTER TABLE video_config ADD COLUMN tv_server_id INTEGER NOT NULL DEFAULT 0"},
		{"tv_dir", "ALTER TABLE video_config ADD COLUMN tv_dir TEXT NOT NULL DEFAULT '/'"},
		{"movie_server_id", "ALTER TABLE video_config ADD COLUMN movie_server_id INTEGER NOT NULL DEFAULT 0"},
		{"movie_dir", "ALTER TABLE video_config ADD COLUMN movie_dir TEXT NOT NULL DEFAULT '/'"},
	} {
		rows, err := DB.Query("PRAGMA table_info(video_config)")
		if err != nil {
			return fmt.Errorf("migrate: pragma table_info(video_config): %w", err)
		}
		exists := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notNull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("migrate: scan table_info(video_config): %w", err)
			}
			if name == c.name {
				exists = true
			}
		}
		rows.Close()
		if !exists {
			if _, err := DB.Exec(c.ddl); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
	}
	// 旧字段 server_id/video_dir 一次性迁移到 tv 列（仅当 tv 列仍为默认值时）
	_, _ = DB.Exec(`UPDATE video_config SET tv_server_id = server_id, tv_dir = video_dir WHERE tv_server_id = 0 AND (server_id != 0 OR video_dir != '/')`)
	// bot_account 渠道扩展列（幂等：仅当列不存在时添加）
	for _, c := range []struct {
		name string
		ddl  string
	}{
		{"channel", "ALTER TABLE bot_account ADD COLUMN channel TEXT NOT NULL DEFAULT 'wechat'"},
		{"app_id", "ALTER TABLE bot_account ADD COLUMN app_id TEXT NOT NULL DEFAULT ''"},
		{"client_secret", "ALTER TABLE bot_account ADD COLUMN client_secret TEXT NOT NULL DEFAULT ''"},
		{"sandbox", "ALTER TABLE bot_account ADD COLUMN sandbox INTEGER NOT NULL DEFAULT 0"},
	} {
		rows, err := DB.Query("PRAGMA table_info(bot_account)")
		if err != nil {
			return fmt.Errorf("migrate: pragma table_info: %w", err)
		}
		exists := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notNull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("migrate: scan table_info: %w", err)
			}
			if name == c.name {
				exists = true
			}
		}
		rows.Close()
		if !exists {
			if _, err := DB.Exec(c.ddl); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
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

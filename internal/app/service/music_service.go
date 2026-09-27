package service

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"strings"
	"time"

	"nas-agent/internal/app/dao"
)

// 支持的音频扩展名
var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".wav": true, ".m4a": true,
	".aac": true, ".ogg": true, ".wma": true, ".ape": true,
}

// MusicService 音乐服务
type MusicService struct{}

// NewMusicService 创建音乐服务
func NewMusicService() *MusicService {
	return &MusicService{}
}

// MusicConfig 音乐配置
type MusicConfig struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"userId"`
	ServerID  int64     `json:"serverId"`
	MusicDir  string    `json:"musicDir"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GetConfig 获取用户的音乐配置
func (s *MusicService) GetConfig(userID int64) (*MusicConfig, error) {
	var cfg MusicConfig
	err := dao.DB.QueryRow(
		`SELECT id, user_id, server_id, music_dir, updated_at FROM music_config WHERE user_id = ?`, userID,
	).Scan(&cfg.ID, &cfg.UserID, &cfg.ServerID, &cfg.MusicDir, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig 保存用户的音乐配置
func (s *MusicService) SaveConfig(userID, serverID int64, musicDir string) error {
	if musicDir == "" {
		musicDir = "/"
	}
	if !strings.HasPrefix(musicDir, "/") {
		musicDir = "/" + musicDir
	}
	_, err := dao.DB.Exec(
		`INSERT INTO music_config(user_id, server_id, music_dir, updated_at) VALUES(?,?,?,datetime('now'))
		 ON CONFLICT(user_id) DO UPDATE SET server_id=excluded.server_id, music_dir=excluded.music_dir, updated_at=datetime('now')`,
		userID, serverID, musicDir,
	)
	return err
}

// SongInfo 歌曲信息
type SongInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	ServerID int64  `json:"serverId"`
	Size     int64  `json:"size"`
}

// PickRandomSong 随机选一首歌
func (s *MusicService) PickRandomSong(userID int64) (*SongInfo, error) {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg.ServerID == 0 {
		return nil, fmt.Errorf("请先配置音乐目录")
	}
	songs, err := s.listSongs(cfg.ServerID, cfg.MusicDir)
	if err != nil {
		return nil, err
	}
	if len(songs) == 0 {
		return nil, fmt.Errorf("音乐目录中没有找到音频文件")
	}
	rand.Seed(time.Now().UnixNano())
	return songs[rand.Intn(len(songs))], nil
}

// SearchSong 按歌名搜索
func (s *MusicService) SearchSong(userID int64, keyword string) (*SongInfo, error) {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg.ServerID == 0 {
		return nil, fmt.Errorf("请先配置音乐目录")
	}
	songs, err := s.listSongs(cfg.ServerID, cfg.MusicDir)
	if err != nil {
		return nil, err
	}
	keyword = strings.ToLower(keyword)
	var matches []*SongInfo
	for _, song := range songs {
		if strings.Contains(strings.ToLower(song.Name), keyword) {
			matches = append(matches, song)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("没有找到包含「%s」的歌曲", keyword)
	}
	// 优先返回最匹配的（名称完全相等 > 前缀匹配 > 包含）
	for _, m := range matches {
		if strings.EqualFold(m.Name, keyword) {
			return m, nil
		}
	}
	for _, m := range matches {
		if strings.HasPrefix(strings.ToLower(m.Name), keyword) {
			return m, nil
		}
	}
	return matches[0], nil
}

// listSongs 递归列出目录下所有音频文件
func (s *MusicService) listSongs(serverID int64, dir string) ([]*SongInfo, error) {
	cli, _, err := openServerClient(serverID)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	var songs []*SongInfo
	var walk func(string) error
	walk = func(d string) error {
		items, err := cli.List(d)
		if err != nil {
			return err
		}
		for _, item := range items {
			fullPath := path.Join(d, item.Name)
			if item.IsDir {
				if err := walk(fullPath); err != nil {
					return err
				}
			} else if audioExts[strings.ToLower(path.Ext(item.Name))] {
				songs = append(songs, &SongInfo{
					Name:     strings.TrimSuffix(item.Name, path.Ext(item.Name)),
					Path:     fullPath,
					ServerID: serverID,
					Size:     item.Size,
				})
			}
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return nil, fmt.Errorf("扫描音乐目录失败: %v", err)
	}
	return songs, nil
}

// LogPlay 记录播放日志
func (s *MusicService) LogPlay(userID int64, chatID, songName, songPath string, serverID int64, status string) error {
	_, err := dao.DB.Exec(
		`INSERT INTO music_play_log(user_id, chat_id, song_name, song_path, server_id, status) VALUES(?,?,?,?,?,?)`,
		userID, chatID, songName, songPath, serverID, status,
	)
	return err
}

// DownloadSong 下载歌曲到本地临时文件（用于发送到微信）
func (s *MusicService) DownloadSong(song *SongInfo) (string, error) {
	cli, _, err := openServerClient(song.ServerID)
	if err != nil {
		return "", fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	rc, err := cli.Download(song.Path)
	if err != nil {
		return "", fmt.Errorf("下载歌曲失败: %v", err)
	}
	defer rc.Close()

	tmpPath := path.Join(os.TempDir(), fmt.Sprintf("nas-agent-music-%d%s", time.Now().UnixNano(), path.Ext(song.Path)))
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", err
	}
	f.Close()
	return tmpPath, nil
}

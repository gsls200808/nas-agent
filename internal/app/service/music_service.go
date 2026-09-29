package service

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"regexp"
	"sort"
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
	Name     string `json:"name"`   // 文件名（去扩展名）
	Title    string `json:"title"`  // 解析出的歌名（去掉歌手部分）
	Artist   string `json:"artist"` // 解析出的歌手（无法解析时为空）
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

// LocalMatch 本地搜索分类结果
type LocalMatch struct {
	Exact []*SongInfo // 歌名完全匹配（不区分大小写），可能同名多首
	Fuzzy []*SongInfo // 歌名/歌手包含关键词但非完全匹配
}

// songSepRe 歌名与歌手分隔符：「歌名 - 歌手」「歌名-歌手」等（半角/全角横线）
var songSepRe = regexp.MustCompile(`\s*[-－–—]\s*`)

// parseTitleArtist 从文件名解析歌名与歌手
func parseTitleArtist(baseName string) (title, artist string) {
	parts := songSepRe.Split(strings.TrimSpace(baseName), 2)
	title = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		artist = strings.TrimSpace(parts[1])
	}
	return
}

// splitSongKeyword 拆分用户输入的「歌名 - 歌手」/「歌手 - 歌名」
func splitSongKeyword(keyword string) []string {
	parts := songSepRe.Split(strings.TrimSpace(keyword), 2)
	out := make([]string, 0, 2)
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ciEq(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func ciContains(a, b string) bool {
	return strings.Contains(strings.ToLower(a), strings.ToLower(b))
}

// SearchLocal 本地分类搜索：
// 支持「歌名 - 歌手」「歌手 - 歌名」两种输入顺序；
// 返回完全匹配（歌名一致）与模糊匹配（仅包含）两组，由调用方决定直接播放还是进入交互菜单。
func (s *MusicService) SearchLocal(userID int64, keyword string) (*LocalMatch, error) {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg.ServerID == 0 {
		return nil, fmt.Errorf("请先配置音乐目录")
	}
	songs, err := s.listSongs(cfg.ServerID, cfg.MusicDir)
	if err != nil {
		return nil, err
	}
	parts := splitSongKeyword(keyword)
	if len(parts) == 0 {
		return &LocalMatch{}, nil
	}

	m := &LocalMatch{}
	seen := map[string]bool{}
	for _, song := range songs {
		var exact, fuzzy bool
		if len(parts) == 2 {
			p1, p2 := parts[0], parts[1]
			t, a := song.Title, song.Artist
			// 两种顺序都算精确匹配
			exact = (ciEq(t, p1) && ciEq(a, p2)) || (ciEq(a, p1) && ciEq(t, p2))
			// 模糊：两部分分别能在歌名或歌手中找到（允许互换）
			if !exact {
				matchP1 := ciContains(t, p1) || ciContains(a, p1) || ciContains(song.Name, p1)
				matchP2 := ciContains(t, p2) || ciContains(a, p2) || ciContains(song.Name, p2)
				fuzzy = matchP1 && matchP2
			}
		} else {
			k := parts[0]
			exact = ciEq(song.Title, k) || ciEq(song.Name, k)
			if !exact {
				fuzzy = ciContains(song.Title, k) || ciContains(song.Name, k) || ciContains(song.Artist, k)
			}
		}
		if exact {
			if !seen[song.Path] {
				m.Exact = append(m.Exact, song)
				seen[song.Path] = true
			}
		} else if fuzzy {
			m.Fuzzy = append(m.Fuzzy, song)
		}
	}

	// 模糊结果排序：歌名前缀优先，其次歌名包含，最后歌手包含
	k := strings.ToLower(parts[0])
	sort.SliceStable(m.Fuzzy, func(i, j int) bool {
		return fuzzyScore(m.Fuzzy[i], k) < fuzzyScore(m.Fuzzy[j], k)
	})
	if len(m.Fuzzy) > 20 {
		m.Fuzzy = m.Fuzzy[:20]
	}
	return m, nil
}

// fuzzyScore 模糊匹配排序分值（越小越靠前）
func fuzzyScore(song *SongInfo, k string) int {
	switch {
	case strings.HasPrefix(strings.ToLower(song.Title), k):
		return 0
	case strings.Contains(strings.ToLower(song.Title), k):
		return 1
	case strings.Contains(strings.ToLower(song.Name), k):
		return 2
	default:
		return 3
	}
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
				base := strings.TrimSuffix(item.Name, path.Ext(item.Name))
				title, artist := parseTitleArtist(base)
				songs = append(songs, &SongInfo{
					Name:     base,
					Title:    title,
					Artist:   artist,
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

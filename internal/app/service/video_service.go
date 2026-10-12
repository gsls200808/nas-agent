package service

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"nas-agent/internal/app/dao"
)

// 支持的视频扩展名
var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true,
	".flv": true, ".wmv": true, ".rmvb": true, ".m4v": true,
	".ts": true, ".webm": true, ".mpg": true, ".mpeg": true,
}

// VideoService 影视服务（电视剧/电影点播，仅本地 NAS 目录，不走网络搜索）
type VideoService struct{}

// NewVideoService 创建影视服务
func NewVideoService() *VideoService {
	return &VideoService{}
}

// VideoConfig 影视目录配置（看电视 / 看电影 各自独立目录）
type VideoConfig struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"userId"`
	TvServerID   int64     `json:"tvServerId"`
	TvDir        string    `json:"tvDir"`
	MovieServerID int64    `json:"movieServerId"`
	MovieDir     string    `json:"movieDir"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// GetConfig 获取用户的影视目录配置
func (s *VideoService) GetConfig(userID int64) (*VideoConfig, error) {
	var cfg VideoConfig
	err := dao.DB.QueryRow(
		`SELECT id, user_id, tv_server_id, tv_dir, movie_server_id, movie_dir, updated_at FROM video_config WHERE user_id = ?`, userID,
	).Scan(&cfg.ID, &cfg.UserID, &cfg.TvServerID, &cfg.TvDir, &cfg.MovieServerID, &cfg.MovieDir, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig 保存用户的影视目录配置（电视 / 电影 各自独立目录与服务器）
func (s *VideoService) SaveConfig(userID, tvServerID int64, tvDir string, movieServerID int64, movieDir string) error {
	tvDir = normDir(tvDir)
	movieDir = normDir(movieDir)
	_, err := dao.DB.Exec(
		`INSERT INTO video_config(user_id, tv_server_id, tv_dir, movie_server_id, movie_dir, updated_at) VALUES(?,?,?,?,?,datetime('now'))
		 ON CONFLICT(user_id) DO UPDATE SET tv_server_id=excluded.tv_server_id, tv_dir=excluded.tv_dir,
		   movie_server_id=excluded.movie_server_id, movie_dir=excluded.movie_dir, updated_at=datetime('now')`,
		userID, tvServerID, tvDir, movieServerID, movieDir,
	)
	return err
}

// normDir 规范化目录路径：空值 → /，确保以 / 开头
func normDir(d string) string {
	if d == "" {
		return "/"
	}
	if !strings.HasPrefix(d, "/") {
		d = "/" + d
	}
	return d
}

// VideoFile 视频文件（电影剧集通用）
type VideoFile struct {
	Name     string `json:"name"`     // 文件名（去扩展名）
	FullName string `json:"fullName"` // 完整文件名（含扩展名）
	Path     string `json:"path"`     // NAS 上的相对路径
	ServerID int64  `json:"serverId"`
	Size     int64  `json:"size"`
}

// ShowFolder 电视剧文件夹
type ShowFolder struct {
	Name string `json:"name"` // 文件夹名（剧名）
	Path string `json:"path"` // NAS 上的相对路径
}

// getTvClient 获取电视剧目录服务器客户端
func (s *VideoService) getTvClient(userID int64) (int64, string, error) {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg.TvServerID == 0 {
		return 0, "", fmt.Errorf("请先配置电视剧目录")
	}
	return cfg.TvServerID, cfg.TvDir, nil
}

// getMovieClient 获取电影目录服务器客户端
func (s *VideoService) getMovieClient(userID int64) (int64, string, error) {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg.MovieServerID == 0 {
		return 0, "", fmt.Errorf("请先配置电影目录")
	}
	return cfg.MovieServerID, cfg.MovieDir, nil
}

// listShowFolders 列出电视剧目录下所有子文件夹（视为剧名）
func (s *VideoService) listShowFolders(userID int64) ([]*ShowFolder, error) {
	serverID, tvDir, err := s.getTvClient(userID)
	if err != nil {
		return nil, err
	}
	cli, _, err := openServerClient(serverID)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	items, err := cli.List(tvDir)
	if err != nil {
		return nil, fmt.Errorf("扫描电视剧目录失败: %v", err)
	}
	var shows []*ShowFolder
	for _, item := range items {
		if item.IsDir {
			shows = append(shows, &ShowFolder{
				Name: item.Name,
				Path: path.Join(tvDir, item.Name),
			})
		}
	}
	return shows, nil
}

// SearchShows 按剧名搜索子文件夹（精确/模糊）
func (s *VideoService) SearchShows(userID int64, keyword string) (exact, fuzzy []*ShowFolder, err error) {
	shows, err := s.listShowFolders(userID)
	if err != nil {
		return nil, nil, err
	}
	kw := strings.ToLower(strings.TrimSpace(keyword))
	for _, sh := range shows {
		if ciEq(sh.Name, kw) {
			exact = append(exact, sh)
		} else if ciContains(sh.Name, kw) {
			fuzzy = append(fuzzy, sh)
		}
	}
	return exact, fuzzy, nil
}

// ListEpisodes 列出指定剧名文件夹下的所有视频文件（按文件名排序）
func (s *VideoService) ListEpisodes(userID int64, showPath string) ([]*VideoFile, error) {
	serverID, _, err := s.getTvClient(userID)
	if err != nil {
		return nil, err
	}
	cli, _, err := openServerClient(serverID)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	items, err := cli.List(showPath)
	if err != nil {
		return nil, fmt.Errorf("扫描剧集目录失败: %v", err)
	}
	var eps []*VideoFile
	for _, item := range items {
		if item.IsDir {
			continue
		}
		if videoExts[strings.ToLower(path.Ext(item.Name))] {
			base := strings.TrimSuffix(item.Name, path.Ext(item.Name))
			eps = append(eps, &VideoFile{
				Name:     base,
				FullName: item.Name,
				Path:     path.Join(showPath, item.Name),
				ServerID: serverID,
				Size:     item.Size,
			})
		}
	}
	sort.Slice(eps, func(i, j int) bool {
		return eps[i].FullName < eps[j].FullName
	})
	return eps, nil
}

// listAllMovies 递归列出电影目录下所有视频文件（电影：平铺所有视频）
func (s *VideoService) listAllMovies(userID int64) ([]*VideoFile, error) {
	serverID, movieDir, err := s.getMovieClient(userID)
	if err != nil {
		return nil, err
	}
	cli, _, err := openServerClient(serverID)
	if err != nil {
		return nil, fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	var movies []*VideoFile
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
			} else if videoExts[strings.ToLower(path.Ext(item.Name))] {
				base := strings.TrimSuffix(item.Name, path.Ext(item.Name))
				movies = append(movies, &VideoFile{
					Name:     base,
					FullName: item.Name,
					Path:     fullPath,
					ServerID: serverID,
					Size:     item.Size,
				})
			}
		}
		return nil
	}
	if err := walk(movieDir); err != nil {
		return nil, fmt.Errorf("扫描电影目录失败: %v", err)
	}
	return movies, nil
}

// SearchMovies 按片名搜索视频文件（精确/模糊，电影平铺）
func (s *VideoService) SearchMovies(userID int64, keyword string) (exact, fuzzy []*VideoFile, err error) {
	movies, err := s.listAllMovies(userID)
	if err != nil {
		return nil, nil, err
	}
	kw := strings.ToLower(strings.TrimSpace(keyword))
	for _, mv := range movies {
		if ciEq(mv.Name, kw) {
			exact = append(exact, mv)
		} else if ciContains(mv.Name, kw) {
			fuzzy = append(fuzzy, mv)
		}
	}
	// 模糊结果最多 20 条
	if len(fuzzy) > 20 {
		fuzzy = fuzzy[:20]
	}
	return exact, fuzzy, nil
}

// DownloadVideo 下载 NAS 上的视频文件到本地临时目录（用于上传夸克）
func (s *VideoService) DownloadVideo(v *VideoFile) (string, error) {
	cli, _, err := openServerClient(v.ServerID)
	if err != nil {
		return "", fmt.Errorf("连接服务器失败: %v", err)
	}
	defer cli.Close()

	rc, err := cli.Download(v.Path)
	if err != nil {
		return "", fmt.Errorf("下载视频失败: %v", err)
	}
	defer rc.Close()

	tmpPath := path.Join(os.TempDir(), fmt.Sprintf("nas-agent-video-%d%s", time.Now().UnixNano(), path.Ext(v.Path)))
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

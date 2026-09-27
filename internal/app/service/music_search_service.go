package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 全盘搜 API（https://so.liumingye.cn 前端对应后端）
const musicSearchAPI = "https://xiageba.liumingye.cn/api/music"

var searchHTTPClient = &http.Client{Timeout: 20 * time.Second}

// MusicSearchService 全盘搜音乐搜索服务
type MusicSearchService struct{}

// NewMusicSearchService 创建搜索服务
func NewMusicSearchService() *MusicSearchService {
	return &MusicSearchService{}
}

// SearchMusicItem 搜索结果条目
type SearchMusicItem struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Artist  string   `json:"artist"`
	Album   string   `json:"album"`
	Quality []string `json:"quality"`
}

// MusicDownload 网盘下载条目
type MusicDownload struct {
	URL     string `json:"url"`     // 夸克分享链接（可能带提取码文本）
	Quality string `json:"quality"` // 如 夸克MP3 / 夸克FLAC
}

// MusicDetail 歌曲详情
type MusicDetail struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Artist    string          `json:"artist"`
	Downloads []MusicDownload `json:"downloads"`
}

// Search 搜索歌曲
func (s *MusicSearchService) Search(keyword string, page, pageSize int) ([]SearchMusicItem, error) {
	q := url.Values{}
	q.Set("q", keyword)
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("pageSize", fmt.Sprintf("%d", pageSize))

	var resp struct {
		Data    []SearchMusicItem `json:"data"`
		Total   int64             `json:"total"`
		Code    int               `json:"code"`
		Message string            `json:"message"`
	}
	if err := s.getJSON(musicSearchAPI+"/search?"+q.Encode(), &resp); err != nil {
		return nil, fmt.Errorf("搜索失败: %w", err)
	}
	return resp.Data, nil
}

// GetDetail 获取歌曲详情（含网盘分享链接）
func (s *MusicSearchService) GetDetail(id string) (*MusicDetail, error) {
	var detail MusicDetail
	if err := s.getJSON(musicSearchAPI+"/"+url.PathEscape(id), &detail); err != nil {
		return nil, fmt.Errorf("获取歌曲详情失败: %w", err)
	}
	return &detail, nil
}

// GetShareURL 获取指定歌曲最优网盘分享链接（优先无损）
func (s *MusicSearchService) GetShareURL(id string) (shareURL, quality string, err error) {
	detail, err := s.GetDetail(id)
	if err != nil {
		return "", "", err
	}
	if len(detail.Downloads) == 0 {
		return "", "", fmt.Errorf("该歌曲没有可用的网盘链接")
	}
	// 优先 FLAC，其次第一个
	best := detail.Downloads[0]
	for _, d := range detail.Downloads {
		if strings.Contains(strings.ToLower(d.Quality), "flac") {
			best = d
			break
		}
	}
	if best.URL == "" {
		return "", "", fmt.Errorf("该歌曲没有可用的网盘链接")
	}
	return best.URL, best.Quality, nil
}

// getJSON GET 请求并解析 JSON
func (s *MusicSearchService) getJSON(reqURL string, target interface{}) error {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://so.liumingye.cn/")

	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, target)
}

package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

// ---- 歌曲宝（gequbao.com）搜索：结果为夸克网盘分享链接，复用夸克转存下载 ----

const gequbaoUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// GequbaoSearchItem 歌曲宝搜索结果
type GequbaoSearchItem struct {
	ID    string `json:"id"`    // 详情页 ID（/music/{id}）
	Title string `json:"title"` // 「歌名 - 歌手」
}

// gequbaoAppData 歌曲宝详情页内联数据
type gequbaoAppData struct {
	Mp3ExtraUrls []struct {
		ShareLink string  `json:"share_link"` // base64 编码的分享链接
		Pwd       *string `json:"pwd"`        // 提取码（可能为空）
		Type      string  `json:"type"`       // 网盘类型，如 夸克网盘
	} `json:"mp3_extra_urls"`
}

var (
	gequbaoSearchRe  = regexp.MustCompile(`(?s)href="(/music/\d+)"[^>]*?title="([^"]*)"`)
	gequbaoAppDataRe = regexp.MustCompile(`(?s)window\.appData = JSON\.parse\('(.+?)'\);`)
	gequbaoDpRe      = regexp.MustCompile(`href="/dp/([A-Za-z0-9+/=]+)"`)
)

// GequbaoSearch 搜索歌曲宝，返回前 limit 条
func (s *MusicSearchService) GequbaoSearch(keyword string, limit int) ([]GequbaoSearchItem, error) {
	body, err := s.gequbaoGet("https://www.gequbao.com/s/" + url.PathEscape(keyword))
	if err != nil {
		return nil, fmt.Errorf("歌曲宝搜索失败: %w", err)
	}
	var out []GequbaoSearchItem
	seen := map[string]bool{}
	for _, m := range gequbaoSearchRe.FindAllStringSubmatch(string(body), -1) {
		id := m[1] // /music/{id}
		if seen[id] {
			continue // 标题链接与下载按钮各出现一次，去重
		}
		seen[id] = true
		out = append(out, GequbaoSearchItem{
			ID:    strings.TrimPrefix(id, "/music/"),
			Title: html.UnescapeString(m[2]),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// GequbaoQuarkLink 获取歌曲宝详情页中的夸克分享链接与提取码
func (s *MusicSearchService) GequbaoQuarkLink(id string) (shareURL, pwd string, err error) {
	body, err := s.gequbaoGet("https://www.gequbao.com/music/" + url.PathEscape(id))
	if err != nil {
		return "", "", fmt.Errorf("获取歌曲宝详情失败: %w", err)
	}
	page := string(body)
	// 优先从内联 appData 提取（含提取码与网盘类型）
	if m := gequbaoAppDataRe.FindStringSubmatch(page); len(m) >= 2 {
		jsonStr := strings.ReplaceAll(m[1], `\u0022`, `"`)
		var ad gequbaoAppData
		if json.Unmarshal([]byte(jsonStr), &ad) == nil {
			for _, u := range ad.Mp3ExtraUrls {
				if !strings.Contains(u.Type, "夸克") {
					continue
				}
				link, derr := decodeB64(u.ShareLink)
				if derr == nil && strings.Contains(link, "pan.quark.cn") {
					if u.Pwd != nil {
						return link, *u.Pwd, nil
					}
					return link, "", nil
				}
			}
		}
	}
	// 备用：/dp/{base64} 下载按钮
	if m := gequbaoDpRe.FindStringSubmatch(page); len(m) >= 2 {
		link, derr := decodeB64(m[1])
		if derr == nil && strings.Contains(link, "pan.quark.cn") {
			return link, "", nil
		}
	}
	return "", "", fmt.Errorf("歌曲宝详情页未找到夸克网盘链接")
}

// gequbaoGet 请求歌曲宝页面
func (s *MusicSearchService) gequbaoGet(reqURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", gequbaoUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", "https://www.gequbao.com/")

	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// decodeB64 base64 解码（兼容缺失 padding）
func decodeB64(s string) (string, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

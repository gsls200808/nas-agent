package service

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/model"
)

// PhaseMerging m3u8 合并阶段
const PhaseMerging = "merging"

// m3u8Progress 下载占 50%，合并占 5%，转存占 45%
const (
	m3u8DownloadWeight = 50
	m3u8MergeWeight    = 5
	m3u8UploadWeight   = 45
)

// isM3U8URL 判断 URL 是否指向 m3u8 资源
func isM3U8URL(u *url.URL) bool {
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u")
}

// m3u8TargetFileName 计算 m3u8 任务的目标文件名（默认取源 URL 去掉 .m3u8 后缀再加 .mp4）
func m3u8TargetFileName(t *model.TransferTask) string {
	if t.TargetFileName == "" {
		base := path.Base(strings.TrimSuffix(t.SourceURL, "/"))
		base = strings.TrimSuffix(base, ".m3u8")
		base = strings.TrimSuffix(base, ".m3u")
		if base == "" || base == "." || base == "/" {
			base = defaultFileName
		}
		return sanitizeFileName(base) + ".mp4"
	}
	if !strings.HasSuffix(strings.ToLower(t.TargetFileName), ".mp4") {
		return t.TargetFileName + ".mp4"
	}
	return t.TargetFileName
}

// downloadM3U8 下载 m3u8 → 下载全部分片 → ffmpeg 合并为 MP4 → 返回本地文件路径与大小
// 进度：下载分片占 m3u8DownloadWeight%，合并占 m3u8MergeWeight%
//
// 已下载完成的分片会被跳过（失败重试时可从断点续跑）；失败时不清理分片目录，
// 以便“从失败步骤开始”重试时复用已下载的分片。
func (s *TransferService) downloadM3U8(cur *model.TransferTask, taskID string) (string, int64, error) {
	// 1. 下载 m3u8 播放列表
	_, segments, err := fetchM3U8Playlist(cur.SourceURL)
	if err != nil {
		return "", 0, fmt.Errorf("解析 m3u8 失败: %v", err)
	}
	if len(segments) == 0 {
		return "", 0, fmt.Errorf("m3u8 无分片")
	}

	s.mutate(taskID, func(t *model.TransferTask) {
		t.Segments = len(segments)
	})

	// 2. 下载全部分片到临时子目录
	segDir := filepath.Join(cur.LocalDir, taskID+"_segs")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("创建分片目录失败: %v", err)
	}
	segFiles := make([]string, 0, len(segments))
	reused := 0
	for i, segURL := range segments {
		segPath := filepath.Join(segDir, fmt.Sprintf("seg_%05d.ts", i))
		if fi, err := os.Stat(segPath); err == nil && fi.Size() > 0 {
			reused++ // 上次已下载完成，直接复用
		} else if err := downloadSegment(segURL, segPath); err != nil {
			return "", 0, fmt.Errorf("下载分片 %d/%d 失败: %v", i+1, len(segments), err)
		}
		segFiles = append(segFiles, segPath)
		s.mutate(taskID, func(t *model.TransferTask) {
			t.DownloadedSegs = i + 1
			t.Progress = (i + 1) * m3u8DownloadWeight / len(segments)
		})
	}
	if reused > 0 {
		util.Logger.Infof("m3u8 任务 %s 续跑：复用 %d/%d 个已下载分片", taskID, reused, len(segments))
	}

	// 3. ffmpeg 合并为 MP4
	s.mutate(taskID, func(t *model.TransferTask) { t.Phase = PhaseMerging })

	// 先合并 TS 分片
	combinedTS := filepath.Join(segDir, "combined.ts")
	if err := concatTSFiles(segFiles, combinedTS); err != nil {
		return "", 0, fmt.Errorf("合并 TS 分片失败: %v", err)
	}

	// 再用 ffmpeg remux 为 MP4
	outputPath := filepath.Join(cur.LocalDir, taskID+"_output.mp4")
	if err := remuxToMP4(combinedTS, outputPath); err != nil {
		return "", 0, err
	}

	// 合并完成，清理分片目录
	_ = os.RemoveAll(segDir)

	fi, _ := os.Stat(outputPath)
	var size int64 = -1
	if fi != nil {
		size = fi.Size()
	}
	s.mutate(taskID, func(t *model.TransferTask) {
		t.Progress = m3u8DownloadWeight + m3u8MergeWeight
		t.Size = size
	})
	util.Logger.Infof("m3u8 合并完成: %s (%d bytes)", outputPath, size)
	return outputPath, size, nil
}

// fetchM3U8 获取 m3u8 播放列表，返回分片 URL 列表
func fetchM3U8Playlist(rawURL string) (string, []string, error) {
	// 下载播放列表内容
	content, err := fetchHTTPText(rawURL)
	if err != nil {
		return "", nil, err
	}
	baseURL, _ := url.Parse(rawURL)
	segments, streamURL, err := parseM3U8Content(content, baseURL)
	if err != nil {
		return "", nil, err
	}
	// 如果是 master playlist，需要继续获取 media playlist
	if streamURL != "" {
		return fetchM3U8Playlist(streamURL)
	}
	return rawURL, segments, nil
}

// parseM3U8Content 解析 m3u8 内容，返回分片列表和（如果是 master）子播放列表 URL
func parseM3U8Content(content string, baseURL *url.URL) (segments []string, streamURL string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	var streamInf bool
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			// master playlist：下一行是 variant stream URL
			streamInf = true
			continue
		}
		if streamInf {
			// 取第一个 variant stream
			resolved := resolveURL(line, baseURL)
			return nil, resolved, nil
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// 普通分片 URL
		resolved := resolveURL(line, baseURL)
		segments = append(segments, resolved)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("解析 m3u8 内容失败: %v", err)
	}
	return segments, "", nil
}

// resolveURL 将相对 URL 解析为绝对 URL
func resolveURL(raw string, baseURL *url.URL) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.IsAbs() {
		return u.String()
	}
	return baseURL.ResolveReference(u).String()
}

// fetchHTTPText 下载 HTTP 内容为文本
func fetchHTTPText(rawURL string) (string, error) {
	resp, err := sourceHTTPClient.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// downloadSegment 下载单个 TS 分片到文件
// 先写入 .part 临时文件，完整下载后再改名，保证最终文件只会是完整的（重试时据此复用）
func downloadSegment(rawURL, localPath string) error {
	resp, err := sourceHTTPClient.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}
	tmpPath := localPath + ".part"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	if err := f.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	return os.Rename(tmpPath, localPath)
}

// concatTSFiles 将多个 TS 分片合并为一个文件（MPEG-TS 支持直接拼接）
func concatTSFiles(segFiles []string, outputPath string) error {
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, seg := range segFiles {
		f, err := os.Open(seg)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return out.Sync()
}

// remuxToMP4 用 ffmpeg 将 TS remux 为 MP4（流复制，不重新编码）
// TS→MP4 时 ffmpeg 会自动插入 aac_adtstoasc 比特流过滤器，无需显式指定
func remuxToMP4(inputTS, outputMP4 string) error {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("m3u8 转 MP4 需要安装 ffmpeg 并加入 PATH")
	}
	cmd := exec.Command(ffmpegPath,
		"-y",          // 覆盖输出
		"-i", inputTS, // 输入
		"-c", "copy", // 流复制，不重新编码
		"-movflags", "+faststart", // mp4 faststart 优化
		outputMP4,
	)
	// 捕获 stderr，失败时附在错误信息中便于排查
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := lastLines(stderr.String(), 3)
		if msg != "" {
			return fmt.Errorf("ffmpeg 转换失败: %v（%s）", err, msg)
		}
		return fmt.Errorf("ffmpeg 转换失败: %v", err)
	}
	return nil
}

// lastLines 取文本末尾 n 行，用于压缩 ffmpeg 的错误输出
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, " | ")
}

package service

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
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

// m3u8Segment 单个分片
type m3u8Segment struct {
	url   string
	ad    bool // 广告分片：下载时跳过
	group int  // 以 EXT-X-DISCONTINUITY 分隔的分组号（启发式判广告用）
}

// m3u8Info 解析 m3u8 后的完整信息
type m3u8Info struct {
	segments    []m3u8Segment // 分片列表（含广告标记）
	keyURI      string        // EXT-X-KEY 的 URI（AES-128 加密时存在）
	keyIV       []byte        // EXT-X-KEY 的 IV（16 字节），无 IV 时由 mediaSequence 推导
	mediaSeq    int           // EXT-X-MEDIA-SEQUENCE，默认 0
	isEncrypted bool          // 是否 AES-128 加密
	adsRemoved  int           // 判定为广告并被剪掉的分片数
}

// downloadM3U8 下载 m3u8 → 下载全部分片（支持 AES-128 解密）→ 纯 Go 转封装为 MP4
// 进度：下载分片占 m3u8DownloadWeight%，合并占 m3u8MergeWeight%
//
// 已下载完成的分片会被跳过（失败重试/暂停续跑时可复用）；失败时不清理分片目录，
// 以便“从失败步骤开始”重试时复用已下载的分片。ctx 取消（暂停/删除）时立即中止。
func (s *TransferService) downloadM3U8(ctx context.Context, cur *model.TransferTask, taskID string) (string, int64, error) {
	// 1. 下载 m3u8 播放列表
	info, err := fetchM3U8Playlist(ctx, cur.SourceURL)
	if err != nil {
		return "", 0, fmt.Errorf("解析 m3u8 失败: %v", err)
	}
	// 剪掉广告分片后的有效分片数（全部判定为广告时直接报错，避免产出空文件）
	total := 0
	for _, seg := range info.segments {
		if !seg.ad {
			total++
		}
	}
	if total == 0 {
		return "", 0, fmt.Errorf("m3u8 无有效分片")
	}
	if info.adsRemoved > 0 {
		util.Logger.Infof("m3u8 任务 %s 剪掉广告分片 %d 个，剩余 %d 个", taskID, info.adsRemoved, total)
	}
	s.mutate(taskID, func(t *model.TransferTask) {
		t.Segments = total
	})

	// 2. 下载全部分片到临时子目录（加密分片先解密再保存）
	segDir := filepath.Join(cur.LocalDir, taskID+"_segs")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("创建分片目录失败: %v", err)
	}

	// 如果有 AES-128 密钥，先下载
	var keyData []byte
	if info.isEncrypted {
		keyURL := resolveURL(info.keyURI, mustParseURL(cur.SourceURL))
		keyData, err = fetchHTTPBytes(ctx, keyURL)
		if err != nil {
			return "", 0, fmt.Errorf("下载解密密钥失败: %v", err)
		}
		if len(keyData) != 16 {
			return "", 0, fmt.Errorf("解密密钥长度错误: %d 字节，期望 16 字节", len(keyData))
		}
		util.Logger.Infof("m3u8 任务 %s 使用 AES-128 加密，已下载密钥", taskID)
	}

	segFiles := make([]string, 0, total)
	reused := 0
	downloaded := 0
	// 注意：跳过广告分片时保留原始序号 i —— 分片文件名稳定（重试可复用），
	// 且 AES-128 无显式 IV 时用 mediaSeq+i 推导，序号必须与播放列表一致
	for i, seg := range info.segments {
		if ctx.Err() != nil { // 暂停/删除任务时中止
			return "", 0, errTaskPaused
		}
		if seg.ad { // 广告分片：不下载、不进成片
			continue
		}
		segPath := filepath.Join(segDir, fmt.Sprintf("seg_%05d.ts", i))
		if fi, err := os.Stat(segPath); err == nil && fi.Size() > 0 {
			reused++ // 上次已下载完成，直接复用
		} else if err := downloadSegment(ctx, seg.url, segPath); err != nil {
			return "", 0, fmt.Errorf("下载分片 %d/%d 失败: %v", downloaded+1, total, err)
		}
		// 解密分片（如果加密）
		if info.isEncrypted && len(keyData) == 16 {
			if err := decryptSegmentFile(segPath, keyData, info.keyIV, info.mediaSeq+i); err != nil {
				return "", 0, fmt.Errorf("解密分片 %d/%d 失败: %v", downloaded+1, total, err)
			}
		}
		segFiles = append(segFiles, segPath)
		downloaded++
		s.mutate(taskID, func(t *model.TransferTask) {
			t.DownloadedSegs = downloaded
			t.Progress = downloaded * m3u8DownloadWeight / total
		})
	}
	if reused > 0 {
		util.Logger.Infof("m3u8 任务 %s 续跑：复用 %d/%d 个已下载分片", taskID, reused, total)
	}

	// 3. 纯 Go 转封装为 MP4（直接读取 TS 分片，无需 ffmpeg，也不用先落盘 combined.ts）
	s.mutate(taskID, func(t *model.TransferTask) { t.Phase = PhaseMerging })

	outputPath := filepath.Join(cur.LocalDir, taskID+"_output.mp4")
	if err := remuxTSToMP4(ctx, segFiles, outputPath); err != nil {
		return "", 0, fmt.Errorf("TS 转 MP4 失败: %v", err)
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

// fetchM3U8Playlist 获取 m3u8 播放列表，返回分片列表及加密信息
func fetchM3U8Playlist(ctx context.Context, rawURL string) (*m3u8Info, error) {
	content, err := fetchHTTPText(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	baseURL, _ := url.Parse(rawURL)
	info, streamURL, err := parseM3U8Content(content, baseURL)
	if err != nil {
		return nil, err
	}
	// 如果是 master playlist，需要继续获取 media playlist
	if streamURL != "" {
		return fetchM3U8Playlist(ctx, streamURL)
	}
	return info, nil
}

// parseM3U8Content 解析 m3u8 内容，返回分片列表和（如果是 master）子播放列表 URL
// 广告识别：优先认 ppvod 注入标记（# ppvod-ad-injected v1 ... #EXT-X-DISCONTINUITY 包裹的广告块）；
// 无标记但存在 DISCONTINUITY 时按「分片目录多数派」启发式判定。判定不了就全部保留（宁可不剪）。
func parseM3U8Content(content string, baseURL *url.URL) (*m3u8Info, string, error) {
	info := &m3u8Info{mediaSeq: 0}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	var streamInf bool
	var inAd bool          // ppvod 广告块内
	var adSegsInBlock int  // 当前广告块已标记的分片数
	var group int          // DISCONTINUITY 分组号
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
		// ppvod 广告注入标记：# ppvod-ad-injected v1 → DISCONTINUITY → 广告分片... → DISCONTINUITY
		if strings.Contains(line, "ppvod-ad-injected") {
			inAd = true
			adSegsInBlock = 0
			continue
		}
		// 解析 EXT-X-KEY
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			parseExtXKey(line, info, baseURL)
			continue
		}
		// 解析 EXT-X-MEDIA-SEQUENCE
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			if seq, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")); err == nil {
				info.mediaSeq = seq
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-DISCONTINUITY") {
			group++
			if inAd && adSegsInBlock > 0 {
				inAd = false // 广告块收尾的 DISCONTINUITY
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// 普通分片 URL
		resolved := resolveURL(line, baseURL)
		seg := m3u8Segment{url: resolved, ad: inAd, group: group}
		if inAd {
			adSegsInBlock++
			info.adsRemoved++
		}
		info.segments = append(info.segments, seg)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("解析 m3u8 内容失败: %v", err)
	}
	// ppvod 标记未命中时，用目录多数派启发式补一刀
	if info.adsRemoved == 0 {
		filterAdsByHeuristic(info)
	}
	return info, "", nil
}

// filterAdsByHeuristic 对没有 ppvod 标记、但含 DISCONTINUITY 的播放列表：
// 统计所有分片 URL 的目录，占绝对多数（> 其余总和）的目录视为正片；
// 与正片目录不同的分片判定为广告。无法判定（无 DISCONTINUITY 分组、目录单一、无过半多数派）时不动。
func filterAdsByHeuristic(info *m3u8Info) {
	if len(info.segments) == 0 {
		return
	}
	dirCount := make(map[string]int)
	maxGroup := 0
	for _, seg := range info.segments {
		dirCount[segmentDir(seg.url)]++
		if seg.group > maxGroup {
			maxGroup = seg.group
		}
	}
	if maxGroup == 0 || len(dirCount) < 2 {
		return // 无 DISCONTINUITY 分组或目录单一，无广告特征
	}
	var mainDir string
	mainCnt := 0
	total := 0
	for d, c := range dirCount {
		total += c
		if c > mainCnt {
			mainCnt, mainDir = c, d
		}
	}
	if mainCnt*2 <= total {
		return // 没有过半多数派，无法可靠区分正片与广告，保守不剪
	}
	for i := range info.segments {
		if segmentDir(info.segments[i].url) != mainDir {
			info.segments[i].ad = true
			info.adsRemoved++
		}
	}
}

// segmentDir 取分片 URL 的目录部分作为分组键
func segmentDir(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return path.Dir(u.Path)
	}
	return raw
}

// parseExtXKey 解析 #EXT-X-KEY 行，提取 URI、METHOD、IV
func parseExtXKey(line string, info *m3u8Info, baseURL *url.URL) {
	attrs := strings.TrimPrefix(line, "#EXT-X-KEY:")
	// 按逗号分割，但 URI 可能在引号内包含逗号，需要简单处理
	var uri, method, ivStr string
	for _, part := range splitKeyAttrs(attrs) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		switch k {
		case "URI":
			uri = v
		case "METHOD":
			method = v
		case "IV":
			ivStr = v
		}
	}
	if method == "AES-128" && uri != "" {
		info.isEncrypted = true
		info.keyURI = uri
		if ivStr != "" {
			// 解析十六进制 IV（去掉 0x 前缀）
			ivStr = strings.TrimPrefix(ivStr, "0x")
			if iv, err := hex.DecodeString(ivStr); err == nil && len(iv) == 16 {
				info.keyIV = iv
			}
		}
	}
}

// splitKeyAttrs 按逗号分割 KEY 属性，但忽略引号内的逗号
func splitKeyAttrs(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		switch r {
		case '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case ',':
			if inQuotes {
				current.WriteRune(r)
			} else {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
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

// fetchHTTPText 下载 HTTP 内容为文本；ctx 取消（暂停/删除）时中止
func fetchHTTPText(ctx context.Context, rawURL string) (string, error) {
	data, err := fetchHTTPBytes(ctx, rawURL)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// fetchHTTPBytes 下载 HTTP 内容为字节；ctx 取消（暂停/删除）时中止
func fetchHTTPBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := sourceHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// downloadSegment 下载单个 TS 分片到文件
// 先写入 .part 临时文件，完整下载后再改名，保证最终文件只会是完整的（重试时据此复用）；
// ctx 取消（暂停/删除）时中止
func downloadSegment(ctx context.Context, rawURL, localPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := sourceHTTPClient.Do(req)
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

// decryptSegmentFile 用 AES-128-CBC 解密分片文件（原地覆盖）
// key: 16 字节密钥；iv: 16 字节 IV（nil 时用 seq 推导）；seq: 分片在 playlist 中的序号
func decryptSegmentFile(segPath string, key, iv []byte, seq int) error {
	data, err := os.ReadFile(segPath)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return fmt.Errorf("分片大小 %d 不是 16 的倍数，可能未加密", len(data))
	}

	// 确定 IV
	var decryptIV []byte
	if iv != nil {
		decryptIV = iv
	} else {
		// 无显式 IV 时，用 media sequence number 作为大端 128 位整数
		decryptIV = make([]byte, 16)
		for i := 0; i < 8; i++ {
			decryptIV[15-i] = byte(seq >> (8 * i))
		}
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	mode := cipher.NewCBCDecrypter(block, decryptIV)
	decrypted := make([]byte, len(data))
	mode.CryptBlocks(decrypted, data)

	// 去除 PKCS#7 填充
	if len(decrypted) == 0 {
		return fmt.Errorf("解密后数据为空")
	}
	padding := int(decrypted[len(decrypted)-1])
	if padding > 0 && padding <= aes.BlockSize && padding <= len(decrypted) {
		// 验证填充是否有效
		valid := true
		for i := len(decrypted) - padding; i < len(decrypted); i++ {
			if decrypted[i] != byte(padding) {
				valid = false
				break
			}
		}
		if valid {
			decrypted = decrypted[:len(decrypted)-padding]
		}
	}

	return os.WriteFile(segPath, decrypted, 0o644)
}

// mustParseURL 解析 URL，失败时返回 nil（调用方需确保 URL 合法）
func mustParseURL(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}

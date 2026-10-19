package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/model"
	"nas-agent/internal/pkg/quark"
)

// 夸克分享转存的单集状态
const (
	episodePending = "pending"
	episodeDone    = "done"
	episodeFailed  = "failed"
)

// quarkEpisodeInterval 集与集之间的间隔，避免连续转存/下载触发夸克风控
const quarkEpisodeInterval = 3 * time.Second

// quarkEpisode 一集视频的转存清单项
type quarkEpisode struct {
	Name   string `json:"name"`
	Fid    string `json:"fid"`
	Token  string `json:"token"`
	Size   int64  `json:"size"`
	Status string `json:"status"` // pending / done / failed
	Error  string `json:"error,omitempty"`
}

// isQuarkShareSource 是否为夸克网盘分享链接
func isQuarkShareSource(s string) bool {
	return strings.Contains(s, "pan.quark.cn") && strings.Contains(s, "/s/")
}

// normalizeSubPath 规范化分享内子目录路径（去掉首尾斜杠与空段）
func normalizeSubPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	var segs []string
	for _, seg := range strings.Split(p, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, "/")
}

// parseQuarkShare 解析分享链接中的 ID 与提取码（兼容纯链接、带提取码文本、?pwd= 参数）
func parseQuarkShare(source string) (pwdID, passcode string, err error) {
	pwdID, passcode, err = quark.ParseShareText(source)
	if err != nil {
		return "", "", err
	}
	if passcode == "" {
		if u, e := url.Parse(source); e == nil {
			passcode = u.Query().Get("pwd")
		}
	}
	return pwdID, passcode, nil
}

// listShareAll 分页拉取分享目录下的全部条目
func listShareAll(cli *quark.Client, pwdID, stoken, pdirFid string) ([]quark.ShareFile, error) {
	const pageSize = 100
	var out []quark.ShareFile
	for page := 1; page <= 50; page++ {
		files, err := cli.GetShareList(pwdID, stoken, pdirFid, page, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
		if len(files) < pageSize {
			break
		}
	}
	return out, nil
}

// locateShareDir 按子目录路径逐层定位分享内的目录 fid（空路径 = 分享根目录 "0"）
func locateShareDir(cli *quark.Client, pwdID, stoken, subPath string) (string, error) {
	fid := "0"
	subPath = normalizeSubPath(subPath)
	if subPath == "" {
		return fid, nil
	}
	for _, seg := range strings.Split(subPath, "/") {
		files, err := listShareAll(cli, pwdID, stoken, fid)
		if err != nil {
			return "", err
		}
		next := ""
		for _, f := range files {
			if f.IsDir && f.Name == seg {
				next = f.Fid
				break
			}
		}
		if next == "" {
			return "", fmt.Errorf("分享中不存在子目录: %s", seg)
		}
		fid = next
	}
	return fid, nil
}

// collectShareVideos 递归收集目录下的视频文件（最多 5 层）
func collectShareVideos(cli *quark.Client, pwdID, stoken, fid string, depth int) ([]quark.ShareFile, error) {
	files, err := listShareAll(cli, pwdID, stoken, fid)
	if err != nil {
		return nil, err
	}
	var out []quark.ShareFile
	for _, f := range files {
		if f.IsDir {
			if depth >= 5 {
				continue
			}
			sub, err := collectShareVideos(cli, pwdID, stoken, f.Fid, depth+1)
			if err != nil {
				util.Logger.Warnf("读取分享子目录 %s 失败: %v", f.Name, err)
				continue
			}
			out = append(out, sub...)
			continue
		}
		if videoExts[strings.ToLower(path.Ext(f.Name))] {
			out = append(out, f)
		}
	}
	return out, nil
}

// QuarkShareDirEntry 分享中的一级目录（供前端交互选择）
type QuarkShareDirEntry struct {
	Name string `json:"name"`
	Fid  string `json:"fid"`
}

// QuarkShareDirView 分享目录浏览结果：子目录列表 + 当前层级直接包含的视频统计
type QuarkShareDirView struct {
	Dirs       []QuarkShareDirEntry `json:"dirs"`
	VideoCount int                  `json:"videoCount"`
	VideoSize  int64                `json:"videoSize"`
}

// ListQuarkShareDirs 浏览夸克分享指定目录（pdirFid 为 "0" 表示分享根目录），
// 供新建任务时交互选择子目录。只返回文件夹与当前层级的视频统计。
func (s *TransferService) ListQuarkShareDirs(userID int64, sourceURL, pdirFid string) (*QuarkShareDirView, error) {
	cli, err := s.quark.getClient(userID)
	if err != nil {
		return nil, err
	}
	pwdID, passcode, err := parseQuarkShare(sourceURL)
	if err != nil {
		return nil, err
	}
	stoken, err := cli.GetShareStoken(pwdID, passcode)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(pdirFid) == "" {
		pdirFid = "0"
	}
	files, err := listShareAll(cli, pwdID, stoken, pdirFid)
	if err != nil {
		return nil, err
	}
	view := &QuarkShareDirView{Dirs: make([]QuarkShareDirEntry, 0)}
	for _, f := range files {
		if f.IsDir {
			view.Dirs = append(view.Dirs, QuarkShareDirEntry{Name: f.Name, Fid: f.Fid})
		} else if videoExts[strings.ToLower(path.Ext(f.Name))] {
			view.VideoCount++
			view.VideoSize += f.Size
		}
	}
	sort.Slice(view.Dirs, func(i, j int) bool { return view.Dirs[i].Name < view.Dirs[j].Name })
	return view, nil
}

// transferQuarkShare 夸克分享子目录滚动转存：
// 列出全部视频集 → 逐集「转存到网盘 → 下载到本地 → 上传目标服务器 → 删除网盘副本」。
// 这样网盘中任意时刻只保留一集，总大小超过网盘容量也能完成转存。
func (s *TransferService) transferQuarkShare(ctx context.Context, cur *model.TransferTask, id string) error {
	cli, err := s.quark.getClient(cur.UserID)
	if err != nil {
		return err
	}
	pwdID, passcode, err := parseQuarkShare(cur.SourceURL)
	if err != nil {
		return err
	}
	stoken, err := cli.GetShareStoken(pwdID, passcode)
	if err != nil {
		return err
	}
	dirFid, err := locateShareDir(cli, pwdID, stoken, cur.SubPath)
	if err != nil {
		return err
	}
	// 续跑：复用已记录的集数清单（已完成的集跳过）；从头开始则重新列举
	var eps []quarkEpisode
	if cur.ResumeStep != "" && cur.Episodes != "" {
		if err := json.Unmarshal([]byte(cur.Episodes), &eps); err != nil {
			eps = nil
		}
	}
	if len(eps) == 0 {
		files, err := collectShareVideos(cli, pwdID, stoken, dirFid, 0)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return fmt.Errorf("分享子目录中没有视频文件")
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		eps = make([]quarkEpisode, 0, len(files))
		for _, f := range files {
			eps = append(eps, quarkEpisode{
				Name:   f.Name,
				Fid:    f.Fid,
				Token:  f.ShareFidToken,
				Size:   f.Size,
				Status: episodePending,
			})
		}
	}

	var total int64
	for _, e := range eps {
		total += e.Size
	}
	s.mutate(id, func(t *model.TransferTask) {
		t.Size = total
		t.TargetPath = cur.TargetDir
	})
	s.saveEpisodes(id, eps)

	dstCli, _, err := openServerClient(cur.TargetServerID)
	if err != nil {
		return fmt.Errorf("连接目标服务器失败: %v", err)
	}
	if err := ensureRemoteDir(dstCli, cur.TargetDir); err != nil {
		dstCli.Close()
		return err
	}
	dstCli.Close() // 仅提前校验可达性；每集上传时新建连接，避免长下载期间连接被对端断开

	var failed []string
	for i := range eps {
		if eps[i].Status == episodeDone {
			continue
		}
		if ctx.Err() != nil {
			return errTaskPaused
		}
		if i > 0 && !sleepCtx(ctx, quarkEpisodeInterval) {
			return errTaskPaused
		}

		// 网盘容量预检：剩余空间放不下这一集时跳过并记录原因（后续重试可继续）
		if free, ok := s.quarkFreeSpace(cli); ok && free < eps[i].Size {
			eps[i].Status = episodeFailed
			eps[i].Error = fmt.Sprintf("网盘剩余空间不足（剩余 %s，需要 %s）", formatBytes(free), formatBytes(eps[i].Size))
			s.saveEpisodes(id, eps)
			failed = append(failed, eps[i].Name)
			continue
		}

		if err := s.transferOneEpisode(ctx, cli, cur, id, pwdID, passcode, &stoken, i, &eps[i]); err != nil {
			if ctx.Err() != nil || errors.Is(err, errTaskPaused) {
				s.saveEpisodes(id, eps)
				return errTaskPaused
			}
			eps[i].Status = episodeFailed
			eps[i].Error = err.Error()
			s.saveEpisodes(id, eps)
			failed = append(failed, eps[i].Name)
			util.Logger.Warnf("夸克分享任务 %s 第 %d 集 %s 失败: %v", id, i+1, eps[i].Name, err)
			continue
		}
		eps[i].Status = episodeDone
		eps[i].Error = ""
		s.saveEpisodes(id, eps)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d/%d 集转存失败：%s", len(failed), len(eps), strings.Join(failed, "、"))
	}
	if ctx.Err() != nil {
		return errTaskPaused
	}
	return nil
}

// transferOneEpisode 单集流程：转存到网盘 → 取下载地址 → 下载到本地 → 上传目标服务器 → 删除网盘副本。
// stoken 过期（41016）时自动刷新重试一次；本地已有完整暂存文件时跳过转存与下载直接上传。
func (s *TransferService) transferOneEpisode(ctx context.Context, cli *quark.Client,
	task *model.TransferTask, id, pwdID, passcode string, stoken *string, idx int, ep *quarkEpisode) error {

	localPath := filepath.Join(task.LocalDir, fmt.Sprintf("%s_ep_%d_%s", task.ID, idx+1, sanitizeFileName(ep.Name)))
	targetPath := path.Join(task.TargetDir, ep.Name)

	// 0. 本地暂存文件已完整（上次下载完成但上传失败的遗留），直接进入上传，不再下载
	if fi, e := os.Stat(localPath); e == nil && ep.Size > 0 && fi.Size() == ep.Size {
		util.Logger.Infof("夸克分享任务 %s 第 %d 集 %s 本地暂存文件已完整（%s），跳过下载直接上传", id, idx+1, ep.Name, formatBytes(fi.Size()))
		s.mutate(id, func(t *model.TransferTask) {
			t.Phase = PhaseTransferring
			t.FileName = ep.Name
			t.LocalPath = localPath
			t.TargetPath = targetPath
		})
		return s.uploadEpisode(ctx, task, localPath, targetPath, ep.Size, id)
	}

	// 1. 转存到自己的网盘根目录（stoken 过期时刷新重试一次）
	fids, err := cli.SaveShareFile(pwdID, *stoken, []string{ep.Fid}, []string{ep.Token})
	if err != nil && isStokenExpired(err) {
		if ns, e := cli.GetShareStoken(pwdID, passcode); e == nil {
			util.Logger.Infof("夸克分享任务 %s 分享凭证已过期，刷新后重试转存（%s）", id, ep.Name)
			*stoken = ns
			fids, err = cli.SaveShareFile(pwdID, ns, []string{ep.Fid}, []string{ep.Token})
		} else {
			err = fmt.Errorf("刷新分享凭证失败: %v（原错误: %v）", e, err)
		}
	}
	if err != nil {
		return err
	}
	if len(fids) == 0 {
		return fmt.Errorf("转存失败：未返回文件 ID")
	}
	// 无论后续成功与否都清理网盘副本，避免占用容量
	defer func() {
		if err := cli.DeleteFiles(fids); err != nil {
			util.Logger.Warnf("删除夸克网盘副本失败（%s）: %v", ep.Name, err)
		}
	}()

	// 2. 获取下载地址
	dlURL, err := cli.GetDownloadURL(fids[0])
	if err != nil {
		return err
	}

	// 3. 下载到本地暂存目录（每集一个临时文件，完成后立即删除）
	s.mutate(id, func(t *model.TransferTask) {
		t.Phase = PhaseDownloading
		t.FileName = ep.Name
		t.LocalPath = localPath
		t.TargetPath = targetPath
	})
	if err := s.downloadEpisode(ctx, cli, dlURL, localPath, id, task.Size); err != nil {
		_ = os.Remove(localPath)
		return err
	}

	// 4. 上传到目标服务器（每集新建连接，下载耗时长、旧连接可能已被对端断开）
	s.mutate(id, func(t *model.TransferTask) { t.Phase = PhaseTransferring })
	return s.uploadEpisode(ctx, task, localPath, targetPath, ep.Size, id)
}

// uploadEpisode 上传本地暂存文件到目标服务器并清理本地文件（size 为该集文件大小）
func (s *TransferService) uploadEpisode(ctx context.Context, task *model.TransferTask, localPath, targetPath string, size int64, id string) error {
	dstCli, _, err := openServerClient(task.TargetServerID)
	if err != nil {
		return fmt.Errorf("连接目标服务器失败: %v", err)
	}
	defer dstCli.Close()
	if err := ensureRemoteDir(dstCli, task.TargetDir); err != nil {
		return err
	}
	if err := s.uploadLocal(ctx, dstCli, localPath, targetPath, size, id); err != nil {
		return err
	}
	if err := os.Remove(localPath); err != nil {
		util.Logger.Warnf("清理本地暂存文件失败（%s）: %v", localPath, err)
	}
	s.mutate(id, func(t *model.TransferTask) { t.LocalPath = "" })
	return nil
}

// isStokenExpired 判断是否为分享凭证过期错误（夸克 code 41016）
func isStokenExpired(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "41016") || strings.Contains(msg, "stoken")
}

// downloadEpisode 下载单集到本地，写入过程支持取消并累加整体进度
func (s *TransferService) downloadEpisode(ctx context.Context, cli *quark.Client, dlURL, localPath, id string, total int64) error {
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("创建本地暂存文件失败: %v", err)
	}
	_, copyErr := cli.DownloadFileTo(dlURL, &ctxCountWriter{
		ctx:     ctx,
		w:       f,
		onWrite: func(n int64) { s.addBytes(id, true, n, total) },
	})
	if copyErr == nil {
		if err := f.Sync(); err != nil {
			copyErr = err
		}
	}
	if err := f.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		if ctx.Err() != nil || errors.Is(copyErr, errTaskPaused) {
			return errTaskPaused
		}
		return fmt.Errorf("下载文件失败: %v", copyErr)
	}
	return nil
}

// saveEpisodes 把集数清单写回任务（同时刷新总集数与已完成集数）
func (s *TransferService) saveEpisodes(id string, eps []quarkEpisode) {
	data, err := json.Marshal(eps)
	if err != nil {
		return
	}
	done := 0
	for _, e := range eps {
		if e.Status == episodeDone {
			done++
		}
	}
	s.mutate(id, func(t *model.TransferTask) {
		t.Episodes = string(data)
		t.Segments = len(eps)
		t.DownloadedSegs = done
	})
}

// quarkFreeSpace 查询网盘剩余容量；查询失败时返回 ok=false（不阻断转存）
func (s *TransferService) quarkFreeSpace(cli *quark.Client) (int64, bool) {
	cap, err := cli.GetCapacity()
	if err != nil {
		util.Logger.Warnf("查询夸克网盘容量失败（跳过容量预检）: %v", err)
		return 0, false
	}
	return cap.Total - cap.Used, true
}

// ctxCountWriter 写入前检查任务是否被取消，并回调已写字节数
type ctxCountWriter struct {
	ctx     context.Context
	w       io.Writer
	onWrite func(int64)
}

func (c *ctxCountWriter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, errTaskPaused
	}
	n, err := c.w.Write(b)
	if n > 0 {
		c.onWrite(int64(n))
	}
	return n, err
}

// sleepCtx 可被取消打断的等待
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// formatBytes 字节数转可读文本
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

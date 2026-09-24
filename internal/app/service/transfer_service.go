package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/common/consts"
	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/dao"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/ftp"
	"nas-agent/internal/app/protocol/spec"
)

// 转存任务状态
const (
	TaskPending = "pending"
	TaskRunning = "running"
	TaskSuccess = "success"
	TaskFailed  = "failed"
)

// 转存任务阶段
const (
	PhasePending      = "pending"      // 排队中
	PhaseDownloading  = "downloading"  // 下载到本地暂存目录
	PhaseTransferring = "transferring" // 转存到目标服务器
	PhaseDone         = "done"         // 已完成
	// PhaseMerging 在 transfer_m3u8.go 中定义
)

// 转存任务步骤（失败后重试时据此判断续跑位置）
const (
	StepDownload = "download" // 下载到本地暂存目录
	StepMerge    = "merge"    // m3u8 合并为 MP4
	StepUpload   = "upload"   // 转存到目标服务器
)

// 重试方式
const (
	RetryRestart = "restart" // 从头开始：清理本地暂存文件重新下载
	RetryResume  = "resume"  // 从失败步骤开始：复用已下载的本地文件
)

const (
	// maxConcurrentTransfer 同时执行的转存任务数
	maxConcurrentTransfer = 3
	// maxTaskHistory 内存中保留的任务记录条数
	maxTaskHistory = 100
	// listTaskLimit 列表接口返回的最大条数
	listTaskLimit = 100
	// persistInterval 进度落库的合并写入间隔
	persistInterval = 2 * time.Second
	// restartInterruptMsg 进程重启导致任务中断时的说明
	restartInterruptMsg = "服务重启导致任务中断，可重试"
	// defaultFileName 下载地址无法解析出文件名时的兜底名称
	defaultFileName = "download"
)

// sourceHTTPClient 下载源专用 HTTP 客户端：不限制总时长（大文件），仅限制响应头超时
var sourceHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// TransferService 转存：从下载地址（http/https/ftp）下载到本地暂存目录 → 转存到目标服务器的存储目录。
// 任务保存在内存中用于执行与实时进度查询，同时持久化到数据库（transfer_task 表），
// 进程重启后仍可查看历史任务并对失败任务重试。
type TransferService struct {
	defaultDir string        // 默认本地暂存目录
	sem        chan struct{} // 并发闸门

	mu    sync.RWMutex
	tasks map[string]*model.TransferTask
	ids   []string        // 任务 ID，新的在前
	dirty map[string]bool // 待落库的任务 ID
}

// NewTransferService 创建转存服务，defaultDir 为空时使用系统临时目录
func NewTransferService(defaultDir string) *TransferService {
	if strings.TrimSpace(defaultDir) == "" {
		defaultDir = os.TempDir()
	}
	s := &TransferService{
		defaultDir: defaultDir,
		sem:        make(chan struct{}, maxConcurrentTransfer),
		tasks:      make(map[string]*model.TransferTask),
		ids:        make([]string, 0, maxTaskHistory),
		dirty:      make(map[string]bool),
	}
	s.interruptUnfinished()
	go s.flushLoop()
	return s
}

// interruptUnfinished 上次进程退出时仍在进行中的任务已无法继续，标记为失败（本地暂存文件保留，可重试）
func (s *TransferService) interruptUnfinished() {
	if err := dao.InterruptUnfinishedTasks(restartInterruptMsg); err != nil {
		util.Logger.Warnf("恢复转存任务状态失败: %v", err)
	}
}

// flushLoop 周期性把内存中的任务状态落库（进度更新频繁，做合并写入）
func (s *TransferService) flushLoop() {
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.flushAll()
	}
}

// flushAll 把标记为待落库的任务批量写入数据库
func (s *TransferService) flushAll() {
	s.mu.Lock()
	pending := make([]*model.TransferTask, 0, len(s.dirty))
	for id := range s.dirty {
		if t, ok := s.tasks[id]; ok {
			cp := *t
			pending = append(pending, &cp)
		}
	}
	s.dirty = make(map[string]bool)
	s.mu.Unlock()

	for _, t := range pending {
		s.save(t)
	}
}

// flush 立即落库单个任务（创建、状态变化、终态时调用）
func (s *TransferService) flush(id string) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	cp := *t
	delete(s.dirty, id)
	s.mu.Unlock()
	s.save(&cp)
}

func (s *TransferService) save(t *model.TransferTask) {
	if err := dao.SaveTransferTask(t); err != nil {
		util.Logger.Warnf("保存转存任务 %s 失败: %v", t.ID, err)
	}
}

// TransferRequest 创建转存任务的入参
type TransferRequest struct {
	UserID         int64  // 发起任务的登录用户，用于记录该用户的近期使用目录
	SourceURL      string // 下载地址（http/https/ftp/m3u8）
	TargetServerID int64  // 目标服务器
	TargetDir      string // 目标服务器上的存储目录
	LocalDir       string // 本地暂存目录，留空用默认值
	TargetFileName string // 用户指定的目标文件名（留空用源文件名）
}

// Create 创建转存任务并异步执行
func (s *TransferService) Create(req TransferRequest) (*model.TransferTask, error) {
	sourceURL := strings.TrimSpace(req.SourceURL)
	if sourceURL == "" {
		return nil, fmt.Errorf("下载地址不能为空")
	}
	u, err := parseSourceURL(sourceURL)
	if err != nil {
		return nil, err
	}
	dst, err := dao.GetServer(req.TargetServerID)
	if err != nil {
		return nil, fmt.Errorf("目标服务器不存在: %d", req.TargetServerID)
	}

	// 判断下载源类型
	sourceType := "http"
	if strings.ToLower(u.Scheme) == "ftp" {
		sourceType = "ftp"
	} else if isM3U8URL(u) {
		sourceType = "m3u8"
	}

	targetDir := strings.TrimSpace(req.TargetDir)
	if targetDir == "" {
		targetDir = "/"
	}
	if !strings.HasPrefix(targetDir, "/") {
		targetDir = "/" + targetDir
	}

	// 用户指定的目标文件名
	targetFileName := sanitizeFileName(strings.TrimSpace(req.TargetFileName))

	localDir := strings.TrimSpace(req.LocalDir)
	if localDir == "" {
		localDir = s.defaultDir
	} else if !filepath.IsAbs(localDir) {
		return nil, fmt.Errorf("本地暂存目录必须是绝对路径: %s", localDir)
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return nil, fmt.Errorf("本地暂存目录不可用: %v", err)
	}

	task := &model.TransferTask{
		ID:               newTaskID(),
		SourceURL:        sourceURL,
		SourceType:       sourceType,
		TargetServerID:   dst.ID,
		TargetServerName: dst.Name,
		TargetDir:        targetDir,
		TargetFileName:   targetFileName,
		LocalDir:         localDir,
		Size:             -1,
		Status:           TaskPending,
		Phase:            PhasePending,
		CreatedAt:        time.Now(),
	}

	s.mu.Lock()
	s.tasks[task.ID] = task
	s.ids = append([]string{task.ID}, s.ids...)
	s.prune()
	s.mu.Unlock()
	s.flush(task.ID)

	// 记录该用户在此目标服务器下使用过的存储目录，供下次快速选择
	if err := dao.SaveRecentDir(req.UserID, dst.ID, targetDir); err != nil {
		util.Logger.Warnf("记录近期使用的目录失败: %v", err)
	}

	go s.run(task.ID)

	cur, _ := s.Get(task.ID)
	return cur, nil
}

// ListRecentDirs 查询某用户在某目标服务器下近期使用的存储目录（最多 10 个，最近使用的在前）
func (s *TransferService) ListRecentDirs(userID, serverID int64) ([]string, error) {
	return dao.ListRecentDirs(userID, serverID)
}

// Get 查询任务快照：内存优先（实时进度），其次数据库中的历史任务
func (s *TransferService) Get(id string) (*model.TransferTask, bool) {
	s.mu.RLock()
	t, ok := s.tasks[id]
	if ok {
		cp := *t
		s.mu.RUnlock()
		return &cp, true
	}
	s.mu.RUnlock()
	if t, err := dao.GetTransferTask(id); err == nil {
		return t, true
	}
	return nil, false
}

// List 任务列表（新的在前）：以内存中的实时状态为准，历史任务从数据库补齐
func (s *TransferService) List() []*model.TransferTask {
	s.mu.RLock()
	mem := make(map[string]*model.TransferTask, len(s.tasks))
	for _, id := range s.ids {
		if t, ok := s.tasks[id]; ok {
			cp := *t
			mem[id] = &cp
		}
	}
	s.mu.RUnlock()

	rows, err := dao.ListTransferTasks(listTaskLimit)
	if err != nil {
		util.Logger.Warnf("读取转存任务列表失败: %v", err)
	}
	out := make([]*model.TransferTask, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, t := range rows {
		if fresh, ok := mem[t.ID]; ok {
			out = append(out, fresh)
			seen[t.ID] = true
			continue
		}
		out = append(out, t)
	}
	// 内存中尚未落库的任务（新建任务落库前的短暂窗口）补在最前
	var extra []*model.TransferTask
	for _, id := range s.ids {
		if t, ok := mem[id]; ok && !seen[id] {
			extra = append(extra, t)
		}
	}
	if len(extra) > 0 {
		out = append(extra, out...)
	}
	if len(out) > listTaskLimit {
		out = out[:listTaskLimit]
	}
	return out
}

// Retry 重试失败的任务。
// mode 为 RetryRestart（从头开始，清理本地暂存文件）或 RetryResume（从失败步骤开始，复用已下载的文件）。
func (s *TransferService) Retry(id, mode string) (*model.TransferTask, error) {
	cur, ok := s.Get(id)
	if !ok {
		return nil, fmt.Errorf("任务不存在")
	}
	if cur.Status != TaskFailed {
		return nil, fmt.Errorf("只有失败的任务可以重试")
	}

	var step string
	switch mode {
	case RetryRestart:
		s.cleanLocal(cur)
	case RetryResume:
		step = cur.FailedStep
		if step == "" {
			return nil, fmt.Errorf("该任务未记录失败步骤，无法从失败步骤继续")
		}
	default:
		return nil, fmt.Errorf("不支持的重试方式: %s", mode)
	}

	// 进程重启前遗留的任务只存在于数据库中，重试前先放回内存表
	s.ensure(cur)

	s.mutate(id, func(t *model.TransferTask) {
		t.Status = TaskPending
		t.Phase = PhasePending
		t.ResumeStep = step
		t.Error = ""
		t.FailedStep = ""
		t.FinishedAt = nil
		t.Uploaded = 0
		if step == StepUpload {
			// 从上传继续：本地文件已就绪，进度回到上传起点
			t.Progress = uploadBaseProgress(t.SourceType)
		}
		if step == "" {
			// 从头开始：清空全部进度
			t.Progress = 0
			t.Downloaded = 0
			t.DownloadedSegs = 0
			t.Segments = 0
			t.Size = -1
			t.LocalPath = ""
			t.TargetPath = ""
		}
	})
	s.flush(id)

	go s.run(id)

	got, _ := s.Get(id)
	return got, nil
}

// ensure 把任务放回内存表（用于重试进程重启前遗留、只存在于数据库中的任务）
func (s *TransferService) ensure(t *model.TransferTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.ID]; ok {
		return
	}
	s.tasks[t.ID] = t
	s.ids = append([]string{t.ID}, s.ids...)
	s.prune()
}

// cleanLocal 清理任务在本地暂存目录中的文件（临时文件、m3u8 分片目录与合并产物）
func (s *TransferService) cleanLocal(t *model.TransferTask) {
	if t.LocalDir != "" {
		_ = os.RemoveAll(filepath.Join(t.LocalDir, t.ID+"_segs"))
		_ = os.Remove(filepath.Join(t.LocalDir, t.ID+"_output.mp4"))
	}
	if t.LocalPath != "" && t.LocalPath != t.LocalDir {
		_ = os.Remove(t.LocalPath)
	}
}

// run 执行任务：拿并发闸门 → 下载 → 转存 → 更新状态
func (s *TransferService) run(id string) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	s.mutate(id, func(t *model.TransferTask) {
		t.Status = TaskRunning
		t.Phase = phaseOfStep(t.ResumeStep)
	})

	if err := s.transfer(id); err != nil {
		s.mutate(id, func(t *model.TransferTask) {
			t.Status = TaskFailed
			t.Error = err.Error()
			// 记录失败步骤，供“从失败步骤开始”重试
			t.FailedStep = stepOfPhase(t.Phase)
			now := time.Now()
			t.FinishedAt = &now
		})
		s.flush(id)
		localPath := ""
		if cur, ok := s.Get(id); ok {
			localPath = cur.LocalPath
			if localPath == "" {
				localPath = cur.LocalDir // m3u8 未生成 MP4 时提示分片所在目录
			}
		}
		util.Logger.Errorf("转存任务 %s 失败: %v（本地暂存文件保留于 %s，可重试）", id, err, localPath)
		return
	}

	s.mutate(id, func(t *model.TransferTask) {
		t.Status = TaskSuccess
		t.Phase = PhaseDone
		t.Progress = 100
		t.FailedStep = ""
		t.ResumeStep = ""
		now := time.Now()
		t.FinishedAt = &now
	})
	s.flush(id)
	util.Logger.Infof("转存任务 %s 完成", id)
}

// stepOfPhase 由当前阶段推断所处步骤
func stepOfPhase(phase string) string {
	switch phase {
	case PhaseMerging:
		return StepMerge
	case PhaseTransferring:
		return StepUpload
	default:
		return StepDownload
	}
}

// phaseOfStep 由续跑步骤推断起始阶段
func phaseOfStep(step string) string {
	switch step {
	case StepMerge:
		return PhaseMerging
	case StepUpload:
		return PhaseTransferring
	default:
		return PhaseDownloading
	}
}

// uploadBaseProgress 转存阶段在总进度中的起始百分比
func uploadBaseProgress(sourceType string) int {
	if sourceType == "m3u8" {
		return m3u8DownloadWeight + m3u8MergeWeight
	}
	return 50
}

// checkLocalReady 校验续跑所需的本地暂存文件是否存在，返回其大小
func checkLocalReady(localPath string) (int64, error) {
	if localPath == "" {
		return 0, fmt.Errorf("该任务未记录本地暂存文件，无法从该步骤继续")
	}
	fi, err := os.Stat(localPath)
	if err != nil || fi.IsDir() {
		return 0, fmt.Errorf("本地暂存文件不存在（%s），无法从该步骤继续", localPath)
	}
	return fi.Size(), nil
}

// transfer 单个任务的完整流程；按 ResumeStep 决定从哪一步继续
func (s *TransferService) transfer(id string) error {
	cur, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("任务不存在")
	}

	var localPath string
	var size int64
	var fileName string
	var err error

	// 续跑：仅“从上传步骤继续”时跳过下载
	// （m3u8 的下载与合并写在同一个方法里，从合并步骤续跑时仍需执行，已下载的分片会被复用）
	skipDownload := cur.ResumeStep == StepUpload

	if cur.SourceType == "m3u8" {
		fileName = m3u8TargetFileName(cur)
		if skipDownload {
			if size, err = checkLocalReady(cur.LocalPath); err != nil {
				return err
			}
			localPath = cur.LocalPath
		} else if localPath, size, err = s.downloadM3U8(cur, id); err != nil {
			// m3u8：下载全部分片 → 合并为 MP4（重试时已下载完成的分片会被跳过）
			return err
		}
	} else if skipDownload {
		fileName = cur.FileName
		if fileName == "" {
			return fmt.Errorf("该任务未记录文件名，无法从该步骤继续")
		}
		if size, err = checkLocalReady(cur.LocalPath); err != nil {
			return err
		}
		localPath = cur.LocalPath
	} else {
		// 直链下载；重试下载步骤时对 http/https 尝试断点续传
		var offset int64
		if cur.ResumeStep == StepDownload && cur.LocalPath != "" {
			if fi, e := os.Stat(cur.LocalPath); e == nil && !fi.IsDir() {
				offset = fi.Size()
			}
		}
		var src io.ReadCloser
		var resumed bool
		src, size, fileName, resumed, err = openSource(cur.SourceURL, offset)
		if err != nil {
			return fmt.Errorf("打开下载地址失败: %v", err)
		}
		defer src.Close()
		if !resumed {
			offset = 0
		}
		// 用户指定了目标文件名则使用
		if cur.TargetFileName != "" {
			fileName = cur.TargetFileName
		}
		localPath = filepath.Join(cur.LocalDir, id+"_"+fileName)
		if offset > 0 && cur.LocalPath != "" {
			// 续传：沿用上次的暂存文件，避免文件名变化导致偏移量错配
			localPath = cur.LocalPath
		}
		s.mutate(id, func(t *model.TransferTask) {
			t.FileName = fileName
			t.Size = size
			t.LocalPath = localPath
			t.TargetPath = path.Join(cur.TargetDir, fileName)
			t.Downloaded = offset // 续传时已有字节数计入进度
		})
		if err := s.downloadToLocal(src, localPath, size, id, offset); err != nil {
			return err
		}
		_ = src.Close()
	}

	// 设置文件名与路径（m3u8 路径需要在此设置，直链路径已在上方设置）
	targetPath := path.Join(cur.TargetDir, fileName)
	s.mutate(id, func(t *model.TransferTask) {
		t.FileName = fileName
		t.LocalPath = localPath
		t.TargetPath = targetPath
		if size > 0 {
			t.Size = size
		}
	})

	// 转存到目标服务器的存储目录（此后任何失败都归入“转存”步骤，重试时无需重新下载）
	s.mutate(id, func(t *model.TransferTask) { t.Phase = PhaseTransferring })

	dstCli, _, err := openServerClient(cur.TargetServerID)
	if err != nil {
		return fmt.Errorf("连接目标服务器失败: %v", err)
	}
	defer dstCli.Close()

	if err := ensureRemoteDir(dstCli, cur.TargetDir); err != nil {
		return err
	}
	if err := s.uploadLocal(dstCli, localPath, targetPath, size, id); err != nil {
		return err
	}
	_ = dstCli.Close()

	// 转存成功，清理本地暂存文件（失败时保留，便于排查）
	if err := os.Remove(localPath); err != nil {
		util.Logger.Warnf("转存任务 %s 已完成，但清理本地暂存文件失败: %v", id, err)
	}
	return nil
}

// downloadToLocal 将下载流写入本地暂存文件；resumeFrom > 0 时追加写入（断点续传）
func (s *TransferService) downloadToLocal(src io.Reader, localPath string, size int64, id string, resumeFrom int64) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if resumeFrom > 0 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(localPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("创建本地暂存文件失败: %v", err)
	}
	_, copyErr := io.Copy(f, &progressReader{r: src, onRead: func(n int64) { s.addBytes(id, true, n, size) }})
	if err := f.Sync(); err != nil && copyErr == nil {
		copyErr = err
	}
	if err := f.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return fmt.Errorf("下载文件失败: %v", copyErr)
	}
	return nil
}

// uploadLocal 将本地暂存文件上传到目标路径
func (s *TransferService) uploadLocal(cli spec.Client, localPath, targetPath string, size int64, id string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地暂存文件失败: %v", err)
	}
	defer f.Close()

	if err := cli.Upload(targetPath, &progressReader{r: f, onRead: func(n int64) { s.addBytes(id, false, n, size) }}, size); err != nil {
		return fmt.Errorf("转存到目标服务器失败: %v", err)
	}
	return nil
}

// mutate 在写锁内更新任务，并标记为待落库（进度更新很频繁，由 flushLoop 合并写入）
func (s *TransferService) mutate(id string, fn func(t *model.TransferTask)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		fn(t)
		s.dirty[id] = true
	}
}

// addBytes 累加传输字节并刷新进度
// 普通下载：下载、上传各占 50%；m3u8：下载分片已在 downloadM3U8 中计算（≤55%），上传占 45%（55→100）
func (s *TransferService) addBytes(id string, downloading bool, n, size int64) {
	s.mutate(id, func(t *model.TransferTask) {
		if downloading {
			t.Downloaded += n
		} else {
			t.Uploaded += n
		}
		if size > 0 {
			if t.SourceType == "m3u8" {
				// 上传阶段：55% + uploaded/size * 45%
				base := m3u8DownloadWeight + m3u8MergeWeight
				p := base + int(t.Uploaded*int64(m3u8UploadWeight)/size)
				if p > 100 {
					p = 100
				}
				if p > t.Progress {
					t.Progress = p
				}
			} else {
				done := t.Downloaded + t.Uploaded
				if done > 2*size {
					done = 2 * size
				}
				t.Progress = int(done * 100 / (2 * size))
			}
		}
	})
}

// prune 裁剪超出保留上限的历史任务。
// 未结束的任务与失败任务不裁剪（失败任务需保留记录与本地文件以便重试），只淘汰最早的成功任务。
func (s *TransferService) prune() {
	for len(s.ids) > maxTaskHistory {
		idx := -1
		for i := len(s.ids) - 1; i >= 0; i-- {
			if t, ok := s.tasks[s.ids[i]]; ok && t.Status == TaskSuccess {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		delete(s.tasks, s.ids[idx])
		s.ids = append(s.ids[:idx], s.ids[idx+1:]...)
	}
}

// progressReader 读取时回调已读字节数
type progressReader struct {
	r      io.Reader
	onRead func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead(int64(n))
	}
	return n, err
}

// ---------- 下载地址（http/https/ftp） ----------

// parseSourceURL 校验并解析下载地址
func parseSourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("下载地址无效: %v", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp":
	case "":
		return nil, fmt.Errorf("下载地址需以 http:// https:// 或 ftp:// 开头: %s", raw)
	default:
		return nil, fmt.Errorf("暂不支持的下载地址协议: %s（支持 http/https/ftp）", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("下载地址缺少主机名: %s", raw)
	}
	if strings.ToLower(u.Scheme) == "ftp" && u.Path == "" {
		return nil, fmt.Errorf("FTP 下载地址缺少文件路径: %s", raw)
	}
	return u, nil
}

// openSource 打开下载地址，返回数据流、文件大小（未知为 -1）、文件名、是否成功续传。
// offset > 0 时表示断点续传（仅 http/https 支持，FTP 不支持则从头下载）
func openSource(raw string, offset int64) (io.ReadCloser, int64, string, bool, error) {
	u, err := parseSourceURL(raw)
	if err != nil {
		return nil, 0, "", false, err
	}
	if strings.ToLower(u.Scheme) == "ftp" {
		rc, size, name, err := openFTPSource(u)
		return rc, size, name, false, err
	}
	return openHTTPSource(u, offset)
}

// openHTTPSource 通过 HTTP(S) GET 打开下载流，offset > 0 时使用 Range 请求续传
func openHTTPSource(u *url.URL, offset int64) (io.ReadCloser, int64, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, "", false, err
	}
	if u.User != nil { // 地址内嵌 user:password 时作为 Basic Auth
		pass, _ := u.User.Password()
		req.SetBasicAuth(u.User.Username(), pass)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := sourceHTTPClient.Do(req)
	if err != nil {
		return nil, 0, "", false, err
	}
	resumed := false
	if offset > 0 {
		switch resp.StatusCode {
		case http.StatusPartialContent:
			resumed = true
		case http.StatusOK:
			offset = 0 // 服务端未按 Range 返回，只能从头下载
		}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, 0, "", false, fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
	}
	size := resp.ContentLength
	if resumed {
		size += offset // 206 的 Content-Length 只是剩余长度
	}
	fileName := fileNameFromURL(u)
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if fn := fileNameFromDisposition(cd); fn != "" {
			fileName = fn
		}
	}
	return resp.Body, size, fileName, resumed, nil
}

// openFTPSource 通过 FTP 客户端打开下载流
func openFTPSource(u *url.URL) (io.ReadCloser, int64, string, error) {
	port := consts.DefaultFTPPort
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, 0, "", fmt.Errorf("FTP 端口无效: %s", p)
		}
		port = n
	}
	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	p := u.Path

	cli := ftp.New(spec.Config{Host: u.Hostname(), Port: port, Username: username, Password: password})
	if err := cli.Open(); err != nil {
		return nil, 0, "", err
	}
	fi, err := cli.Stat(p)
	if err != nil {
		_ = cli.Close()
		return nil, 0, "", fmt.Errorf("读取 FTP 文件信息失败: %v", err)
	}
	if fi.IsDir {
		_ = cli.Close()
		return nil, 0, "", fmt.Errorf("FTP 地址指向目录: %s", p)
	}
	rc, err := cli.Download(p)
	if err != nil {
		_ = cli.Close()
		return nil, 0, "", err
	}
	fileName := sanitizeFileName(fi.Name)
	if fileName == "" {
		fileName = fileNameFromURL(u)
	}
	return &streamCloser{rc: rc, cli: cli}, fi.Size, fileName, nil
}

// fileNameFromURL 从地址路径取文件名
func fileNameFromURL(u *url.URL) string {
	fileName := sanitizeFileName(path.Base(u.Path))
	if fileName == "" {
		fileName = defaultFileName
	}
	return fileName
}

// fileNameFromDisposition 解析 Content-Disposition 中的文件名
func fileNameFromDisposition(cd string) string {
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return sanitizeFileName(params["filename"])
}

// sanitizeFileName 规范文件名，过滤路径与本地文件系统非法字符
func sanitizeFileName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "" || name == "." || name == ".." || name == "/" {
		return ""
	}
	return strings.NewReplacer(`\`, "_", "/", "_", ":", "_", "*", "_",
		"?", "_", `"`, "_", "<", "_", ">", "_", "|", "_").Replace(name)
}

// ensureRemoteDir 确保远端目录存在（逐级创建）
func ensureRemoteDir(cli spec.Client, dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "/" {
		return nil
	}
	if fi, err := cli.Stat(dir); err == nil && fi.IsDir {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		cur += "/" + seg
		if fi, err := cli.Stat(cur); err == nil && fi.IsDir {
			continue
		}
		if err := cli.Mkdir(cur); err != nil {
			if fi, e := cli.Stat(cur); e == nil && fi.IsDir {
				continue
			}
			return fmt.Errorf("创建目标目录 %s 失败: %v", cur, err)
		}
	}
	return nil
}

// newTaskID 生成任务 ID
func newTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

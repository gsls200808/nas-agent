package smb

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/common/util"
	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/spec"
)

var _ spec.Client = (*Client)(nil)

// NTSTATUS
const (
	statusSuccess            = 0x00000000
	statusMoreProcessing     = 0xC0000016
	statusNoMoreFiles        = 0x80000006
	statusObjectNameNotFound = 0xC0000034
	statusAccessDenied       = 0xC0000022
	statusEndOfFile          = 0xC0000011
)

// SMB2 命令
const (
	cmdNegotiate      = 0x0000
	cmdSessionSetup   = 0x0001
	cmdTreeConnect    = 0x0003
	cmdTreeDisconnect = 0x0004
	cmdCreate         = 0x0005
	cmdClose          = 0x0006
	cmdRead           = 0x0008
	cmdWrite          = 0x0009
	cmdSetInfo        = 0x000B
	cmdQueryDirectory = 0x000E
)

const (
	protocolID = 0x424D53FE // "\xfeSMB"
	headerLen  = 64
	maxRW      = 64 * 1024 // AirDisk MaxReadSize/MaxWriteSize 均为 64KB，请求过大会报 INVALID_PARAMETER
)

// 访问掩码/创建参数
const (
	fileGenericRead  = 0x00120089 // FILE_GENERIC_READ
	fileGenericWrite = 0x00120116 // FILE_GENERIC_WRITE
	deleteAccess     = 0x00010000
	shareAll         = 0x00000007
	attrNormal       = 0x80
	attrDirectory    = 0x10
	dispOpen         = 1
	dispCreate       = 2
	dispOverwriteIf  = 5
	optDir           = 0x00000001
	optNonDir        = 0x00000040
	optDeleteOnClose = 0x00001000
)

// smbLog 输出诊断日志（util.Logger 未初始化时静默跳过）
func smbLog(format string, args ...interface{}) {
	if util.Logger != nil {
		util.Logger.Infof("[smb] "+format, args...)
	}
}

// Client 纯标准库实现的 SMB2/3 客户端（方言 2.0.2 ~ 3.0.2，NTLMv2 认证）
type Client struct {
	cfg       spec.Config
	conn      net.Conn
	mu        sync.Mutex
	msgID     uint64
	sessionID uint64
	treeID    uint32
}

// New 创建 SMB 客户端
func New(cfg spec.Config) *Client {
	return &Client{cfg: cfg}
}

// Open 连接、协商、认证、连接共享
func (c *Client) Open() error {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		smbLog("dial %s failed: %v", addr, err)
		return fmt.Errorf("smb dial %s: %w", addr, err)
	}
	c.conn = conn
	if err := c.negotiateSMB1(); err != nil {
		smbLog("SMB1 negotiate failed: %v", err)
		_ = conn.Close()
		return fmt.Errorf("smb negotiate: %w", err)
	}
	if err := c.negotiate(); err != nil {
		smbLog("negotiate failed: %v", err)
		_ = conn.Close()
		return err
	}
	if err := c.sessionSetup(); err != nil {
		smbLog("session setup failed: %v", err)
		_ = conn.Close()
		return err
	}
	if err := c.treeConnect(); err != nil {
		smbLog("tree connect failed: %v", err)
		_ = conn.Close()
		return err
	}
	return nil
}

// Close 断开
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	_, _, _ = c.roundTrip(cmdTreeDisconnect, make([]byte, 4), nil)
	err := c.conn.Close()
	c.conn = nil
	return err
}

// ---------- 传输层 ----------

func (c *Client) roundTrip(command uint16, body, buffer []byte) (uint32, []byte, error) {
	c.msgID++
	total := headerLen + len(body) + len(buffer)
	msg := make([]byte, total)
	binary.LittleEndian.PutUint32(msg[0:4], protocolID)
	binary.LittleEndian.PutUint16(msg[4:6], headerLen)
	binary.LittleEndian.PutUint16(msg[12:14], command)
	binary.LittleEndian.PutUint16(msg[14:16], 128) // credits
	binary.LittleEndian.PutUint64(msg[24:32], c.msgID)
	binary.LittleEndian.PutUint32(msg[36:40], c.treeID)
	binary.LittleEndian.PutUint64(msg[40:48], c.sessionID)
	copy(msg[headerLen:], body)
	copy(msg[headerLen+len(body):], buffer)

	nb := []byte{0x00, byte(total >> 16), byte(total >> 8), byte(total)}
	if _, err := c.conn.Write(append(nb, msg...)); err != nil {
		return 0, nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return 0, nil, err
	}
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	resp := make([]byte, n)
	if _, err := io.ReadFull(c.conn, resp); err != nil {
		return 0, nil, err
	}
	if len(resp) < headerLen {
		return 0, nil, fmt.Errorf("smb: short response")
	}
	status := binary.LittleEndian.Uint32(resp[8:12])
	return status, resp, nil
}

// ntStatusName 常见 NTSTATUS 名称（便于诊断）
var ntStatusName = map[uint32]string{
	0x00000000: "STATUS_SUCCESS",
	0xC0000016: "STATUS_MORE_PROCESSING_REQUIRED",
	0x80000006: "STATUS_NO_MORE_FILES",
	0xC0000005: "STATUS_ACCESS_VIOLATION",
	0xC0000022: "STATUS_ACCESS_DENIED",
	0xC0000034: "STATUS_OBJECT_NAME_NOT_FOUND",
	0xC0000035: "STATUS_OBJECT_NAME_COLLISION",
	0xC000003A: "STATUS_OBJECT_PATH_NOT_FOUND",
	0xC0000043: "STATUS_SHARING_VIOLATION",
	0xC000006A: "STATUS_WRONG_PASSWORD",
	0xC0000064: "STATUS_NO_SUCH_USER",
	0xC000006D: "STATUS_LOGON_FAILURE",
	0xC0000071: "STATUS_PASSWORD_EXPIRED",
	0xC0000072: "STATUS_ACCOUNT_DISABLED",
	0xC000015B: "STATUS_LOGON_TYPE_NOT_GRANTED",
	0xC00000BB: "STATUS_NOT_SUPPORTED",
	0xC0000002: "STATUS_NOT_IMPLEMENTED",
	0xC000000D: "STATUS_INVALID_PARAMETER",
	0xC000A000: "STATUS_INVALID_SIGNATURE",
	0xC000019B: "STATUS_SHARE_REDIRECTED",
	0xC00000CC: "STATUS_BAD_NETWORK_NAME",
	0xC0000205: "STATUS_INSUFF_SERVER_RESOURCES",
}

func statusErr(status uint32, op string) error {
	if name, ok := ntStatusName[status]; ok {
		return fmt.Errorf("smb: %s failed, ntstatus=0x%08X(%s)", op, status, name)
	}
	return fmt.Errorf("smb: %s failed, ntstatus=0x%08X", op, status)
}

// ---------- NEGOTIATE ----------

// negotiateSMB1 发送 SMB1 NEGOTIATE 引导请求（部分设备如 AirDisk 收到裸 SMB2 会直接 EOF）
func (c *Client) negotiateSMB1() error {
	dialectList := []string{"NT LM 0.12", "SMB 2.002", "SMB 2.???"}
	dialects := []byte{}
	for _, d := range dialectList {
		dialects = append(dialects, 0x02)
		dialects = append(dialects, []byte(d)...)
		dialects = append(dialects, 0x00)
	}
	msg := make([]byte, 32)
	copy(msg[0:4], []byte{0xFF, 'S', 'M', 'B'})
	msg[4] = 0x72 // SMB1 NEGOTIATE
	msg[9] = 0x18 // flags
	binary.LittleEndian.PutUint16(msg[10:12], 0xC853) // flags2
	binary.LittleEndian.PutUint16(msg[28:30], 0xFEFF) // pid
	binary.LittleEndian.PutUint16(msg[30:32], 1)      // mid
	body := []byte{0x00}                              // wordcount
	body = append(body, byte(len(dialects)), byte(len(dialects)>>8))
	body = append(body, dialects...)
	payload := append(msg, body...)

	n := len(payload)
	nb := []byte{0x00, byte(n >> 16), byte(n >> 8), byte(n)}
	if _, err := c.conn.Write(append(nb, payload...)); err != nil {
		return err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return err
	}
	respLen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	resp := make([]byte, respLen)
	if _, err := io.ReadFull(c.conn, resp); err != nil {
		return err
	}
	smbLog("SMB1 negotiate response: %d bytes, proto=%02x", respLen, resp[0])
	return nil
}

func (c *Client) negotiate() error {
	// 方言为 uint16 数组；3.1.1 需要 Negotiate Context（预认证完整性），暂不提供
	dialects := []uint16{0x0202, 0x0210, 0x0300, 0x0302} // 2.0.2 / 2.1 / 3.0 / 3.0.2
	body := make([]byte, 36+len(dialects)*2)
	binary.LittleEndian.PutUint16(body[0:2], 36)
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(dialects)))
	binary.LittleEndian.PutUint16(body[4:6], 1) // SecurityMode: signing enabled not required
	// Capabilities 保持 0：不声明 DFS / 加密等能力
	guid := make([]byte, 16)
	_, _ = rand.Read(guid)
	copy(body[12:28], guid)
	for i, d := range dialects {
		binary.LittleEndian.PutUint16(body[36+i*2:], d)
	}
	status, resp, err := c.roundTrip(cmdNegotiate, body, nil)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "negotiate")
	}
	secMode := binary.LittleEndian.Uint16(resp[headerLen+2:])
	dialect := binary.LittleEndian.Uint16(resp[headerLen+4:])
	smbLog("negotiate ok: dialect=0x%04X securityMode=0x%02X signingRequired=%v",
		dialect, secMode, secMode&0x02 != 0)
	switch dialect {
	case 0x0202, 0x0210, 0x0300, 0x0302:
		return nil
	}
	return fmt.Errorf("smb: unsupported dialect negotiated: 0x%04X", dialect)
}

// ---------- SESSION_SETUP ----------

func (c *Client) sessionSetup() error {
	domain := c.cfg.Workgroup
	if domain == "" {
		domain = "WORKGROUP"
	}
	// 第一轮：Type1（AirDisk 等低端设备不支持 SPNEGO，直接发裸 NTLMSSP）
	status, resp, err := c.sessionSetupRequest(type1())
	if err != nil {
		return err
	}
	if status == statusSuccess {
		smbLog("session setup ok (guest/anonymous)")
		return nil // 匿名/来宾
	}
	if status != statusMoreProcessing {
		return statusErr(status, "session setup")
	}
	raw := extractNTLM(c.securityBuffer(resp))
	if raw == nil {
		return fmt.Errorf("smb: no NTLMSSP challenge in response")
	}
	info, err := parseType2(raw)
	if err != nil {
		return err
	}
	if sid := binary.LittleEndian.Uint64(resp[40:48]); sid != 0 {
		c.sessionID = sid
	}
	// 第二轮：Type3（裸 NTLMSSP）
	status, _, err = c.sessionSetupRequest(
		type3(c.cfg.Username, c.cfg.Password, domain, "", info))
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "ntlm authenticate")
	}
	smbLog("ntlm authenticate ok: user=%s domain=%s sessionID=%d", c.cfg.Username, domain, c.sessionID)
	return nil
}

func (c *Client) sessionSetupRequest(gssToken []byte) (uint32, []byte, error) {
	// StructureSize=25 含 Buffer 首字节，固定部分物理长度 24 字节，安全缓冲紧跟其后（偏移 88）
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body[0:2], 25)
	body[3] = 1 // SecurityMode
	binary.LittleEndian.PutUint16(body[12:14], headerLen+24)
	binary.LittleEndian.PutUint16(body[14:16], uint16(len(gssToken)))
	return c.roundTrip(cmdSessionSetup, body, gssToken)
}

// securityBuffer 读取响应中的安全缓冲
func (c *Client) securityBuffer(resp []byte) []byte {
	off := int(binary.LittleEndian.Uint16(resp[headerLen+4:]))
	n := int(binary.LittleEndian.Uint16(resp[headerLen+6:]))
	if off == 0 || n == 0 || off+n > len(resp) {
		return nil
	}
	return resp[off : off+n]
}

// ---------- TREE_CONNECT ----------

func (c *Client) treeConnect() error {
	share := strings.Trim(c.cfg.Share, `\/`)
	if share == "" {
		return fmt.Errorf("smb: share name required")
	}
	name := utf16LE(fmt.Sprintf(`\\%s\%s`, c.cfg.Host, share))
	// StructureSize=9 含 Path 首字节，固定部分物理长度 8 字节，路径紧跟其后（偏移 72）
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:2], 9)
	binary.LittleEndian.PutUint16(body[4:6], headerLen+8)
	binary.LittleEndian.PutUint16(body[6:8], uint16(len(name)))
	status, resp, err := c.roundTrip(cmdTreeConnect, body, name)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "tree connect")
	}
	c.treeID = binary.LittleEndian.Uint32(resp[36:40])
	smbLog("tree connect ok: \\\\%s\\%s treeID=%d", c.cfg.Host, share, c.treeID)
	return nil
}

// ---------- CREATE / CLOSE ----------

// create 打开/创建文件或目录，返回 16 字节 FileId
func (c *Client) create(name string, isDir bool, access, disposition, options uint32) ([]byte, error) {
	var name16 []byte
	if name != "" {
		name16 = utf16LE(name)
	}
	attrs := uint32(0)
	if disposition == dispCreate || disposition == dispOverwriteIf {
		attrs = attrNormal
		if isDir {
			attrs = attrDirectory
		}
	}
	// AirDisk 要求 body 物理 64 字节（末尾 8 字节 CreateContexts 补零），
	// 且文件名紧跟在 56 字节固定部分之后（NameOffset=120），8 字节补零放在名字之后
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:2], 57)
	// body[2]=SecurityFlags, body[3]=RequestedOplockLevel 均为 0
	binary.LittleEndian.PutUint32(body[4:8], 2) // ImpersonationLevel = Identification
	binary.LittleEndian.PutUint32(body[24:28], access)
	binary.LittleEndian.PutUint32(body[28:32], attrs)
	binary.LittleEndian.PutUint32(body[32:36], shareAll)
	binary.LittleEndian.PutUint32(body[36:40], disposition)
	binary.LittleEndian.PutUint32(body[40:44], options)
	binary.LittleEndian.PutUint16(body[44:46], headerLen+56)
	binary.LittleEndian.PutUint16(body[46:48], uint16(len(name16)))
	// CreateContextsOffset/Length 保持 0；buffer = 文件名 + 8 字节补零
	buffer := make([]byte, len(name16)+8)
	copy(buffer, name16)
	status, resp, err := c.roundTrip(cmdCreate, body, buffer)
	if err != nil {
		return nil, err
	}
	if status != statusSuccess {
		return nil, statusErr(status, "create "+name)
	}
	fileID := make([]byte, 16)
	copy(fileID, resp[headerLen+64:headerLen+80])
	return fileID, nil
}

func (c *Client) close(fileID []byte) error {
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body[0:2], 24)
	copy(body[8:24], fileID)
	status, _, err := c.roundTrip(cmdClose, body, nil)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "close")
	}
	return nil
}

// ---------- 路径转换 ----------

// smbName 将虚拟路径（相对共享根，/ 分隔）转为 SMB 相对名（\ 分隔）
func smbName(p string) string {
	p = path.Clean("/" + p)
	if p == "/" || p == "." {
		return ""
	}
	return strings.TrimPrefix(strings.ReplaceAll(p, "/", `\`), `\`)
}

// dirBase 返回相对父目录（\ 分隔）与基名
func dirBase(p string) (string, string) {
	p = path.Clean("/" + p)
	if p == "/" {
		return "", ""
	}
	dir := path.Dir(p)
	name := path.Base(p)
	if dir == "/" {
		return "", name
	}
	return strings.TrimPrefix(strings.ReplaceAll(dir, "/", `\`), `\`), name
}

// ---------- List ----------

func (c *Client) queryDirOne(fileID []byte, rewind bool) (uint32, []byte, error) {
	pattern := utf16LE("*")
	body := make([]byte, 32)
	binary.LittleEndian.PutUint16(body[0:2], 33)
	body[2] = 38 // FileIdFullDirectoryInformation（AirDisk 固件固定返回此格式）
	if rewind {
		body[3] = 0x01 // SL_RESTART_SCAN
	}
	copy(body[8:24], fileID)
	binary.LittleEndian.PutUint16(body[24:26], headerLen+32)
	binary.LittleEndian.PutUint16(body[26:28], uint16(len(pattern)))
	binary.LittleEndian.PutUint32(body[28:32], 65536)
	return c.roundTrip(cmdQueryDirectory, body, pattern)
}

// List 列目录
func (c *Client) List(dir string) ([]model.FileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, fmt.Errorf("smb: not connected")
	}
	fileID, err := c.create(smbName(dir), true, fileGenericRead, dispOpen, optDir)
	if err != nil {
		return nil, err
	}
	defer c.close(fileID)

	var out []model.FileInfo
	rewind := true
	for {
		status, resp, err := c.queryDirOne(fileID, rewind)
		rewind = false
		if err != nil {
			return nil, err
		}
		if status == statusNoMoreFiles {
			break
		}
		if status != statusSuccess {
			return nil, statusErr(status, "query directory")
		}
		bufOff := int(binary.LittleEndian.Uint16(resp[headerLen+2:]))
		bufLen := int(binary.LittleEndian.Uint32(resp[headerLen+4:]))
		if bufOff+bufLen > len(resp) {
			break
		}
		out = append(out, parseBothDir(resp[bufOff:bufOff+bufLen])...)
	}
	result := make([]model.FileInfo, 0, len(out))
	for _, fi := range out {
		if fi.Name == "." || fi.Name == ".." {
			continue
		}
		fi.Path = path.Join(path.Clean("/"+dir), fi.Name)
		result = append(result, fi)
	}
	return result, nil
}

// parseBothDir 解析 FileIdFullDirectoryInformation 链（MS-FSCC 2.4.18，文件名在偏移 80）
func parseBothDir(buf []byte) []model.FileInfo {
	var out []model.FileInfo
	off := 0
	for off+80 <= len(buf) {
		next := int(binary.LittleEndian.Uint32(buf[off:]))
		lastWrite := binary.LittleEndian.Uint64(buf[off+24:])
		endOfFile := int64(binary.LittleEndian.Uint64(buf[off+40:]))
		attrs := binary.LittleEndian.Uint32(buf[off+56:])
		nameLen := int(binary.LittleEndian.Uint32(buf[off+60:]))
		nameBytes := buf[off+80:]
		if nameLen > len(nameBytes) {
			break
		}
		out = append(out, model.FileInfo{
			Name:    decodeUTF16(nameBytes[:nameLen]),
			IsDir:   attrs&attrDirectory != 0,
			Size:    endOfFile,
			ModTime: filetimeToTime(lastWrite),
		})
		if next == 0 {
			break
		}
		off += next
	}
	return out
}

// Stat 通过列父目录匹配
func (c *Client) Stat(p string) (*model.FileInfo, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return &model.FileInfo{Name: "/", Path: "/", IsDir: true, ModTime: time.Now()}, nil
	}
	dir, name := dirBase(p)
	list, err := c.List("/" + strings.ReplaceAll(dir, `\`, "/"))
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			list[i].Path = p
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("smb: %s not found", p)
}

// ---------- Download ----------

type smbReader struct {
	c      *Client
	fileID []byte
	offset uint64
	buf    []byte
	bufPos int
	eof    bool
	closed bool
}

func (r *smbReader) Read(p []byte) (int, error) {
	if r.bufPos >= len(r.buf) {
		if r.eof {
			return 0, io.EOF
		}
		n, err := r.readChunk()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf[r.bufPos:])
	r.bufPos += n
	return n, nil
}

func (r *smbReader) readChunk() (int, error) {
	// 单次读取限制 64KB：AirDisk 固件 MaxReadSize 较小，请求过大会返回 INVALID_PARAMETER
	const maxRead = 64 * 1024
	body := make([]byte, 49)
	binary.LittleEndian.PutUint16(body[0:2], 49)
	binary.LittleEndian.PutUint32(body[4:8], uint32(maxRead))
	binary.LittleEndian.PutUint64(body[8:16], r.offset)
	copy(body[16:32], r.fileID)
	status, resp, err := r.c.roundTrip(cmdRead, body, nil)
	if err != nil {
		return 0, err
	}
	// AirDisk 固件在读到文件末尾时返回 STATUS_END_OF_FILE 而非成功+0 字节，视为正常 EOF
	if status == statusEndOfFile {
		r.eof = true
		r.buf = nil
		return 0, nil
	}
	if status != statusSuccess {
		return 0, statusErr(status, "read")
	}
	dataOff := int(resp[headerLen+2])
	dataLen := int(binary.LittleEndian.Uint32(resp[headerLen+4:]))
	if dataOff+dataLen > len(resp) || dataLen == 0 {
		r.eof = true
		r.buf = nil
		return 0, nil
	}
	// 复制一份，避免下次 roundTrip 覆盖底层数组
	r.buf = append([]byte{}, resp[dataOff:dataOff+dataLen]...)
	r.bufPos = 0
	r.offset += uint64(dataLen)
	return dataLen, nil
}

func (r *smbReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.c.close(r.fileID)
	r.c.mu.Unlock()
	return err
}

// Download 打开文件并返回数据流（mutex 由 smbReader.Close 释放）
func (c *Client) Download(p string) (io.ReadCloser, error) {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("smb: not connected")
	}
	fileID, err := c.create(smbName(p), false, fileGenericRead, dispOpen, optNonDir)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	return &smbReader{c: c, fileID: fileID}, nil
}

// ---------- Upload ----------

// Upload 上传文件
func (c *Client) Upload(p string, r io.Reader, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("smb: not connected")
	}
	fileID, err := c.create(smbName(p), false, fileGenericRead|fileGenericWrite|deleteAccess,
		dispOverwriteIf, optNonDir)
	if err != nil {
		return err
	}
	defer c.close(fileID)

	buf := make([]byte, maxRW)
	var offset uint64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if werr := c.writeChunk(fileID, buf[:n], offset); werr != nil {
				return werr
			}
			offset += uint64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) writeChunk(fileID, data []byte, offset uint64) error {
	// StructureSize=49 含首数据字节，固定部分物理 48 字节，数据紧跟其后（偏移 112）
	const bodyLen = 48
	const dataOff = headerLen + bodyLen
	body := make([]byte, bodyLen)
	binary.LittleEndian.PutUint16(body[0:2], 49)
	binary.LittleEndian.PutUint16(body[2:4], dataOff)
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(data)))
	binary.LittleEndian.PutUint64(body[8:16], offset)
	copy(body[16:32], fileID)
	status, resp, err := c.roundTrip(cmdWrite, body, data)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "write")
	}
	if written := binary.LittleEndian.Uint32(resp[headerLen+4:]); int(written) != len(data) {
		return fmt.Errorf("smb: short write %d/%d", written, len(data))
	}
	return nil
}

// ---------- Mkdir / Remove / Rename ----------

// Mkdir 创建目录
func (c *Client) Mkdir(p string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fileID, err := c.create(smbName(p), true, fileGenericRead|fileGenericWrite, dispCreate, optDir)
	if err != nil {
		return err
	}
	return c.close(fileID)
}

// Remove 删除文件或目录（以 DELETE_ON_CLOSE 方式打开后关闭）
func (c *Client) Remove(p string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fileID, err := c.create(smbName(p), false, fileGenericRead|deleteAccess, dispOpen,
		optNonDir|optDeleteOnClose)
	if err != nil {
		fileID, err = c.create(smbName(p), true, fileGenericRead|deleteAccess, dispOpen,
			optDir|optDeleteOnClose)
		if err != nil {
			return err
		}
	}
	return c.close(fileID)
}

// Rename 重命名/移动（SET_INFO FileRenameInformation）
func (c *Client) Rename(from, to string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fileID, err := c.create(smbName(from), false, fileGenericRead|fileGenericWrite|deleteAccess, dispOpen,
		optNonDir)
	if err != nil {
		fileID, err = c.create(smbName(from), true, fileGenericRead|fileGenericWrite|deleteAccess, dispOpen,
			optDir)
		if err != nil {
			return err
		}
	}
	defer c.close(fileID)

	newName := utf16LE(smbName(to))
	setBuf := make([]byte, 24+len(newName))
	setBuf[0] = 1 // ReplaceIfExists
	binary.LittleEndian.PutUint64(setBuf[8:16], 0)
	binary.LittleEndian.PutUint32(setBuf[16:20], uint32(len(newName)))
	copy(setBuf[24:], newName)

	body := make([]byte, 32)
	binary.LittleEndian.PutUint16(body[0:2], 33)
	body[2] = 1  // InfoType = FILE
	body[3] = 10 // FileRenameInformation
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(setBuf)))
	binary.LittleEndian.PutUint16(body[8:10], headerLen+32)
	copy(body[16:32], fileID)
	status, _, err := c.roundTrip(cmdSetInfo, body, setBuf)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "rename")
	}
	return nil
}

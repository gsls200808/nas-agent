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
	maxRW      = 1024 * 1024
)

// 访问掩码/创建参数
const (
	genericRead      = 0x80000000
	genericWrite     = 0x40000000
	deleteAccess     = 0x00010000
	shareAll         = 0x00000007
	attrNormal       = 0x80
	attrDirectory    = 0x10
	dispOpen         = 1
	dispCreate       = 2
	dispOverwriteIf  = 5
	optDir           = 0x00000001
	optNonDir        = 0x00000040
	optSyncAlert     = 0x00000020
	optDeleteOnClose = 0x00001000
)

// Client 纯标准库实现的 SMB2 客户端（方言 2.0.2/2.1，NTLMv2 认证）
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
		return fmt.Errorf("smb dial: %w", err)
	}
	c.conn = conn
	if err := c.negotiate(); err != nil {
		_ = conn.Close()
		return err
	}
	if err := c.sessionSetup(); err != nil {
		_ = conn.Close()
		return err
	}
	if err := c.treeConnect(); err != nil {
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

func statusErr(status uint32, op string) error {
	return fmt.Errorf("smb: %s failed, ntstatus=0x%08X", op, status)
}

// ---------- NEGOTIATE ----------

func (c *Client) negotiate() error {
	dialects := []uint32{0x0202, 0x0210} // SMB 2.0.2, SMB 2.1
	body := make([]byte, 36+len(dialects)*4)
	binary.LittleEndian.PutUint16(body[0:2], 36)
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(dialects)))
	binary.LittleEndian.PutUint16(body[4:6], 1) // SecurityMode: signing enabled not required
	binary.LittleEndian.PutUint32(body[8:12], 0x7F)
	guid := make([]byte, 16)
	_, _ = rand.Read(guid)
	copy(body[12:28], guid)
	for i, d := range dialects {
		binary.LittleEndian.PutUint32(body[36+i*4:], d)
	}
	status, resp, err := c.roundTrip(cmdNegotiate, body, nil)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "negotiate")
	}
	dialect := binary.LittleEndian.Uint16(resp[headerLen+4:])
	if dialect != 0x0202 && dialect != 0x0210 {
		return fmt.Errorf("smb: unsupported dialect negotiated: 0x%04X", dialect)
	}
	return nil
}

// ---------- SESSION_SETUP ----------

func (c *Client) sessionSetup() error {
	domain := c.cfg.Workgroup
	if domain == "" {
		domain = "WORKGROUP"
	}
	// 第一轮：Type1
	status, resp, err := c.sessionSetupRequest(wrapNegTokenInit(type1()))
	if err != nil {
		return err
	}
	if status == statusSuccess {
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
	// 第二轮：Type3
	status, _, err = c.sessionSetupRequest(wrapNegTokenResp(
		type3(c.cfg.Username, c.cfg.Password, domain, "", info)))
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "ntlm authenticate")
	}
	return nil
}

func (c *Client) sessionSetupRequest(gssToken []byte) (uint32, []byte, error) {
	body := make([]byte, 25)
	binary.LittleEndian.PutUint16(body[0:2], 25)
	body[3] = 1 // SecurityMode
	binary.LittleEndian.PutUint16(body[12:14], headerLen+25)
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
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], 9)
	binary.LittleEndian.PutUint16(body[4:6], headerLen+9)
	binary.LittleEndian.PutUint16(body[6:8], uint16(len(name)))
	status, resp, err := c.roundTrip(cmdTreeConnect, body, name)
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return statusErr(status, "tree connect")
	}
	c.treeID = binary.LittleEndian.Uint32(resp[36:40])
	return nil
}

// ---------- CREATE / CLOSE ----------

// create 打开/创建文件或目录，返回 16 字节 FileId
func (c *Client) create(name string, isDir bool, access, disposition, options uint32) ([]byte, error) {
	var name16 []byte
	if name != "" {
		name16 = utf16LE(name)
	}
	attrs := uint32(attrNormal)
	if isDir {
		attrs = attrDirectory
	}
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:2], 57)
	body[3] = 2 // ImpersonationLevel = Impersonation
	binary.LittleEndian.PutUint32(body[20:24], access)
	binary.LittleEndian.PutUint32(body[24:28], attrs)
	binary.LittleEndian.PutUint32(body[28:32], shareAll)
	binary.LittleEndian.PutUint32(body[32:36], disposition)
	binary.LittleEndian.PutUint32(body[36:40], options)
	binary.LittleEndian.PutUint16(body[40:42], headerLen+56)
	binary.LittleEndian.PutUint16(body[42:44], uint16(len(name16)))
	status, resp, err := c.roundTrip(cmdCreate, body, name16)
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
	body[2] = 3 // FileBothDirectoryInformation
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
	fileID, err := c.create(smbName(dir), true, genericRead, dispOpen, optDir|optSyncAlert)
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

// parseBothDir 解析 FileBothDirectoryInformation 链（MS-FSCC 2.4.8）
func parseBothDir(buf []byte) []model.FileInfo {
	var out []model.FileInfo
	off := 0
	for off+94 <= len(buf) {
		next := int(binary.LittleEndian.Uint32(buf[off:]))
		lastWrite := binary.LittleEndian.Uint64(buf[off+24:])
		endOfFile := int64(binary.LittleEndian.Uint64(buf[off+40:]))
		attrs := binary.LittleEndian.Uint32(buf[off+56:])
		nameLen := int(binary.LittleEndian.Uint32(buf[off+60:]))
		nameBytes := buf[off+94:]
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
	body := make([]byte, 49)
	binary.LittleEndian.PutUint16(body[0:2], 49)
	binary.LittleEndian.PutUint32(body[4:8], uint32(maxRW))
	binary.LittleEndian.PutUint64(body[8:16], r.offset)
	copy(body[16:32], r.fileID)
	status, resp, err := r.c.roundTrip(cmdRead, body, nil)
	if err != nil {
		return 0, err
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

// Download 打开文件并返回数据流
func (c *Client) Download(p string) (io.ReadCloser, error) {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("smb: not connected")
	}
	fileID, err := c.create(smbName(p), false, genericRead, dispOpen, optNonDir|optSyncAlert)
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
	fileID, err := c.create(smbName(p), false, genericRead|genericWrite|deleteAccess,
		dispOverwriteIf, optNonDir|optSyncAlert)
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
	// body 补齐到 56 字节，使数据相对报文起始 8 字节对齐（64+56=120）
	const bodyLen = 56
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
	fileID, err := c.create(smbName(p), true, genericRead|genericWrite, dispCreate, optDir|optSyncAlert)
	if err != nil {
		return err
	}
	return c.close(fileID)
}

// Remove 删除文件或目录（以 DELETE_ON_CLOSE 方式打开后关闭）
func (c *Client) Remove(p string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fileID, err := c.create(smbName(p), false, genericRead|deleteAccess, dispOpen,
		optNonDir|optSyncAlert|optDeleteOnClose)
	if err != nil {
		fileID, err = c.create(smbName(p), true, genericRead|deleteAccess, dispOpen,
			optDir|optSyncAlert|optDeleteOnClose)
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
	fileID, err := c.create(smbName(from), false, genericRead|genericWrite|deleteAccess, dispOpen,
		optNonDir|optSyncAlert)
	if err != nil {
		fileID, err = c.create(smbName(from), true, genericRead|genericWrite|deleteAccess, dispOpen,
			optDir|optSyncAlert)
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

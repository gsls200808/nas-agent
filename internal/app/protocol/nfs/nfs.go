package nfs

import (
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"nas-agent/internal/app/model"
	"nas-agent/internal/app/protocol/spec"
)

var _ spec.Client = (*Client)(nil)

// NFS3 过程号（RFC 1813）
const (
	procGetAttr = 1
	procLookup  = 3
	procRead    = 6
	procWrite   = 7
	procCreate  = 8
	procMkDir   = 9
	procRemove  = 12
	procRmDir   = 13
	procRename  = 14
	procReaddir = 16
)

const (
	nf3Reg           = 1
	nf3Dir           = 2
	mountOK          = 0
	nfs3OK           = 0
	stableFileSync   = 2
	createUnchecked  = 0
	cookieVerfSize   = 8
	nfsRWChunk       = 256 * 1024
)

// Client 纯标准库实现的 NFSv3 客户端（TCP，AUTH_SYS，经 portmap 发现 mountd）
type Client struct {
	cfg        spec.Config
	nfsPort    uint32
	mountdPort uint32
	rootFH     []byte

	cacheMu  sync.Mutex
	fhCache  map[string][]byte // 虚拟目录路径 -> 文件句柄
}

// New 创建 NFS 客户端
func New(cfg spec.Config) *Client {
	return &Client{fhCache: make(map[string][]byte)}
}

// Open 发现端口、挂载导出、校验根目录
func (c *Client) Open() error {
	pmAddr := fmt.Sprintf("%s:111", c.cfg.Host)
	nfsPort, err := getport(pmAddr, progNFS, 3)
	if err != nil {
		return err
	}
	if nfsPort == 0 {
		nfsPort = 2049
	}
	c.nfsPort = nfsPort

	mountdPort, err := getport(pmAddr, progMountd, 3)
	if err != nil {
		return fmt.Errorf("nfs: query mountd port: %w", err)
	}
	if mountdPort == 0 {
		return fmt.Errorf("nfs: mountd port not registered (NFSv3 MOUNT service unavailable)")
	}
	c.mountdPort = mountdPort

	export := strings.TrimSpace(c.cfg.Share)
	if export == "" {
		export = "/"
	}
	fh, err := c.mount(export)
	if err != nil {
		return err
	}
	c.rootFH = fh
	c.cacheSet("/", append([]byte{}, fh...))
	return nil
}

// Close 无持久连接，空实现
func (c *Client) Close() error { return nil }

// mount 调用 MOUNTD MNTPROC_MNT(1)
//
//	struct mountres3_ok {
//	    fhandle3 fhandle;                 // opaque<64>
//	    auth_flavor auth_flavors<>;
//	};
func (c *Client) mount(export string) ([]byte, error) {
	w := newXDRWriter()
	w.writeString(export)
	r, err := rpcCall(fmt.Sprintf("%s:%d", c.cfg.Host, c.mountdPort), progMountd, 3, 1, w.bytes())
	if err != nil {
		return nil, err
	}
	st, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	if st != mountOK {
		return nil, fmt.Errorf("nfs: mount %s failed, status=%d", export, st)
	}
	fh, err := r.readOpaque()
	if err != nil {
		return nil, err
	}
	n, err := r.readUint32() // auth flavors 数量
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < n; i++ {
		if _, err := r.readUint32(); err != nil {
			return nil, err
		}
	}
	return fh, nil
}

// nfsCall 调用 NFS 程序，返回已读过状态码的 reader
func (c *Client) nfsCall(proc uint32, args []byte) (*xdrReader, uint32, error) {
	r, err := rpcCall(fmt.Sprintf("%s:%d", c.cfg.Host, c.nfsPort), progNFS, 3, proc, args)
	if err != nil {
		return nil, 0, err
	}
	st, err := r.readUint32()
	if err != nil {
		return nil, 0, err
	}
	return r, st, nil
}

// ---------- fattr3 / wcc_data / post_op_fh3 ----------

type fattr3 struct {
	ftype uint32
	mode  uint32
	size  int64
	mtime time.Time
}

// readFattr 解析 fattr3（固定 84 字节）
//
//	ftype mode nlink uid gid (5×u32)
//	size used rdev fsid fileid (8+8+8+8+8)
//	atime mtime ctime (3×8)
func readFattr(r *xdrReader) (fattr3, error) {
	var f fattr3
	var err error
	if f.ftype, err = r.readUint32(); err != nil {
		return f, err
	}
	if f.mode, err = r.readUint32(); err != nil {
		return f, err
	}
	for i := 0; i < 3; i++ { // nlink uid gid
		if _, err = r.readUint32(); err != nil {
			return f, err
		}
	}
	size, err := r.readUint64()
	if err != nil {
		return f, err
	}
	f.size = int64(size)
	if _, err = r.readUint64(); err != nil { // used
		return f, err
	}
	if _, err = r.readFixedOpaque(8); err != nil { // rdev (specdata3)
		return f, err
	}
	if _, err = r.readUint64(); err != nil { // fsid
		return f, err
	}
	if _, err = r.readUint64(); err != nil { // fileid
		return f, err
	}
	if _, err = r.readUint32(); err != nil { // atime.seconds
		return f, err
	}
	if _, err = r.readUint32(); err != nil { // atime.nseconds
		return f, err
	}
	msec, err := r.readUint32()
	if err != nil {
		return f, err
	}
	mnsec, err := r.readUint32()
	if err != nil {
		return f, err
	}
	f.mtime = time.Unix(int64(msec), int64(mnsec))
	if _, err = r.readUint32(); err != nil { // ctime
		return f, err
	}
	if _, err = r.readUint32(); err != nil {
		return f, err
	}
	return f, nil
}

// skipPostOpAttrs 解析 post_op_attr：bool + fattr3
func skipPostOpAttrs(r *xdrReader) error {
	ok, err := r.readBool()
	if err != nil {
		return err
	}
	if ok {
		_, err = readFattr(r)
	}
	return err
}

// skipPostOpFH 解析 post_op_fh3：bool + nfs_fh3
func skipPostOpFH(r *xdrReader) error {
	ok, err := r.readBool()
	if err != nil {
		return err
	}
	if ok {
		_, err = r.readOpaque()
	}
	return err
}

// skipWcc 解析 wcc_data：before_op_attr + after_op_attr
func skipWcc(r *xdrReader) error {
	if err := skipPostOpAttrs(r); err != nil {
		return err
	}
	return skipPostOpAttrs(r)
}

// ---------- 句柄/路径 ----------

func (c *Client) cacheGet(p string) ([]byte, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	fh, ok := c.fhCache[p]
	return fh, ok
}

func (c *Client) cacheSet(p string, fh []byte) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.fhCache[p] = fh
}

// invalidateCache 使某路径及其子路径缓存失效
func (c *Client) invalidateCache(p string) {
	p = path.Clean("/" + p)
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	for k := range c.fhCache {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(c.fhCache, k)
		}
	}
}

// lookupPath 返回任意路径的句柄（带缓存）
//
//	LOOKUP3resok {
//	    nfs_fh3 object;
//	    post_op_attr obj_attributes;
//	    post_op_fh3  dir_attributes;
//	}
func (c *Client) lookupPath(p string) ([]byte, error) {
	p = path.Clean("/" + p)
	if fh, ok := c.cacheGet(p); ok {
		return fh, nil
	}
	parent := path.Dir(p)
	base := path.Base(p)
	parentFH, err := c.lookupPath(parent)
	if err != nil {
		return nil, err
	}
	w := newXDRWriter()
	w.writeOpaque(parentFH)
	w.writeString(base)
	r, st, err := c.nfsCall(procLookup, w.bytes())
	if err != nil {
		return nil, err
	}
	if st != nfs3OK {
		return nil, fmt.Errorf("nfs: lookup %s failed, status=%d", p, st)
	}
	fh, err := r.readOpaque()
	if err != nil {
		return nil, err
	}
	if err := skipPostOpAttrs(r); err != nil {
		return nil, err
	}
	if err := skipPostOpFH(r); err != nil {
		return nil, err
	}
	c.cacheSet(p, fh)
	return fh, nil
}

// lookupParent 返回 p 所在目录的句柄与基名
func (c *Client) lookupParent(p string) ([]byte, string, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return append([]byte{}, c.rootFH...), "", nil
	}
	fh, err := c.lookupPath(path.Dir(p))
	if err != nil {
		return nil, "", err
	}
	return fh, path.Base(p), nil
}

// ---------- GETATTR / Stat ----------

func (c *Client) getAttr(fh []byte) (fattr3, error) {
	w := newXDRWriter()
	w.writeOpaque(fh)
	r, st, err := c.nfsCall(procGetAttr, w.bytes())
	if err != nil {
		return fattr3{}, err
	}
	if st != nfs3OK {
		return fattr3{}, fmt.Errorf("nfs: getattr failed, status=%d", st)
	}
	return readFattr(r)
}

// Stat 查询条目
func (c *Client) Stat(p string) (*model.FileInfo, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		fa, err := c.getAttr(append([]byte{}, c.rootFH...))
		if err != nil {
			return nil, err
		}
		return &model.FileInfo{Name: "/", Path: "/", IsDir: fa.ftype == nf3Dir, Size: fa.size, ModTime: fa.mtime}, nil
	}
	fh, err := c.lookupPath(p)
	if err != nil {
		return nil, err
	}
	fa, err := c.getAttr(fh)
	if err != nil {
		return nil, err
	}
	return &model.FileInfo{
		Name:    path.Base(p),
		Path:    p,
		IsDir:   fa.ftype == nf3Dir,
		Size:    fa.size,
		ModTime: fa.mtime,
	}, nil
}

// ---------- READDIR / List ----------

// List 列目录
//
//	READDIR3args { nfs_fh3 dir; cookie3; cookieverf3[8]; count3 count }
//	READDIR3resok { post_op_attr dir_attributes;
//	                cookieverf3 cookieverf;
//	                entry3 *entries; bool eof }
//	entry3 { fileid3; filename3 name; cookie3 cookie; next* }
func (c *Client) List(dir string) ([]model.FileInfo, error) {
	dir = path.Clean("/" + dir)
	dirFH, err := c.lookupPath(dir)
	if err != nil {
		return nil, err
	}
	var out []model.FileInfo
	cookie := uint64(0)
	cookieVerf := make([]byte, cookieVerfSize)
	first := true
	for {
		w := newXDRWriter()
		w.writeOpaque(dirFH)
		w.writeUint64(cookie)
		if first {
			w.writeFixedOpaque(make([]byte, cookieVerfSize))
		} else {
			w.writeFixedOpaque(cookieVerf)
		}
		w.writeUint32(65536) // count

		r, st, err := c.nfsCall(procReaddir, w.bytes())
		if err != nil {
			return nil, err
		}
		if st != nfs3OK {
			return nil, fmt.Errorf("nfs: readdir failed, status=%d", st)
		}
		if err := skipPostOpAttrs(r); err != nil {
			return nil, err
		}
		cookieVerf, err = r.readFixedOpaque(cookieVerfSize)
		if err != nil {
			return nil, err
		}
		type entry struct {
			name   string
			cookie uint64
		}
		var entries []entry
		for {
			follows, err := r.readBool()
			if err != nil {
				return nil, err
			}
			if !follows {
				break
			}
			if _, err = r.readUint64(); err != nil { // fileid
				return nil, err
			}
			name, err := r.readString()
			if err != nil {
				return nil, err
			}
			ck, err := r.readUint64()
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry{name: name, cookie: ck})
		}
		eof, err := r.readBool()
		if err != nil {
			return nil, err
		}
		// READDIR 不返回属性，逐个 GETATTR
		for _, e := range entries {
			childPath := path.Join(dir, e.name)
			fi, err := c.Stat(childPath)
			if err != nil {
				out = append(out, model.FileInfo{Name: e.name, Path: childPath, ModTime: time.Now()})
				continue
			}
			out = append(out, *fi)
		}
		if eof || len(entries) == 0 {
			break
		}
		cookie = entries[len(entries)-1].cookie
		first = false
	}
	return out, nil
}

// ---------- READ / Download ----------

type nfsReader struct {
	c      *Client
	fh     []byte
	offset uint64
	buf    []byte
	pos    int
	eof    bool
}

func (r *nfsReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.fetch(); err != nil {
			return 0, err
		}
		if r.pos >= len(r.buf) {
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

// fetch READ3resok { post_op_attr; count3 count; bool eof; opaque data<> }
func (r *nfsReader) fetch() error {
	w := newXDRWriter()
	w.writeOpaque(r.fh)
	w.writeUint64(r.offset)
	w.writeUint32(nfsRWChunk)
	resp, st, err := r.c.nfsCall(procRead, w.bytes())
	if err != nil {
		return err
	}
	if st != nfs3OK {
		return fmt.Errorf("nfs: read failed, status=%d", st)
	}
	if err := skipPostOpAttrs(resp); err != nil {
		return err
	}
	count, err := resp.readUint32()
	if err != nil {
		return err
	}
	r.eof, err = resp.readBool()
	if err != nil {
		return err
	}
	data, err := resp.readOpaque()
	if err != nil {
		return err
	}
	if uint32(len(data)) != count {
		return fmt.Errorf("nfs: read length mismatch %d/%d", len(data), count)
	}
	r.buf = data
	r.pos = 0
	r.offset += uint64(len(data))
	return nil
}

func (r *nfsReader) Close() error { return nil }

// Download 下载文件
func (c *Client) Download(p string) (io.ReadCloser, error) {
	fh, err := c.lookupPath(path.Clean("/" + p))
	if err != nil {
		return nil, err
	}
	return &nfsReader{c: c, fh: fh}, nil
}

// ---------- WRITE / Upload ----------

// Upload 上传文件（CREATE + WRITE FILE_SYNC）
func (c *Client) Upload(p string, r io.Reader, size int64) error {
	p = path.Clean("/" + p)
	dirFH, name, err := c.lookupParent(p)
	if err != nil {
		return err
	}
	fileFH, err := c.createFile(dirFH, name)
	if err != nil {
		return err
	}
	defer c.invalidateCache(p)

	buf := make([]byte, nfsRWChunk)
	var offset uint64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if err := c.writeChunk(fileFH, buf[:n], offset); err != nil {
				return err
			}
			offset += uint64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// createFile CREATE3(UNCHECKED)
//
//	CREATE3resok { post_op_fh3 obj; post_op_attr obj_attributes; wcc_data dir_wcc }
func (c *Client) createFile(dirFH []byte, name string) ([]byte, error) {
	w := newXDRWriter()
	w.writeOpaque(dirFH)
	w.writeString(name)
	w.writeUint32(createUnchecked)
	w.buf = append(w.buf, emptySattr3()...)
	r, st, err := c.nfsCall(procCreate, w.bytes())
	if err != nil {
		return nil, err
	}
	if st != nfs3OK {
		return nil, fmt.Errorf("nfs: create failed, status=%d", st)
	}
	follows, err := r.readBool()
	if err != nil {
		return nil, err
	}
	if !follows {
		return nil, fmt.Errorf("nfs: create returned no file handle")
	}
	fh, err := r.readOpaque()
	if err != nil {
		return nil, err
	}
	if err := skipPostOpAttrs(r); err != nil {
		return nil, err
	}
	if err := skipWcc(r); err != nil {
		return nil, err
	}
	return fh, nil
}

// writeChunk WRITE3resok { wcc_data file_wcc; count3 count; stable_how committed; cookieverf3 verify[8] }
func (c *Client) writeChunk(fh, data []byte, offset uint64) error {
	w := newXDRWriter()
	w.writeOpaque(fh)
	w.writeUint64(offset)
	w.writeUint32(uint32(len(data)))
	w.writeUint32(stableFileSync)
	w.writeOpaque(data)
	r, st, err := c.nfsCall(procWrite, w.bytes())
	if err != nil {
		return err
	}
	if st != nfs3OK {
		return fmt.Errorf("nfs: write failed, status=%d", st)
	}
	if err := skipWcc(r); err != nil {
		return err
	}
	count, err := r.readUint32()
	if err != nil {
		return err
	}
	if count != uint32(len(data)) {
		return fmt.Errorf("nfs: short write %d/%d", count, len(data))
	}
	if _, err = r.readUint32(); err != nil { // committed
		return err
	}
	if _, err = r.readFixedOpaque(cookieVerfSize); err != nil {
		return err
	}
	return nil
}

// ---------- Mkdir / Remove / Rename ----------

// emptySattr3 全部字段“不设置”的 sattr3（mode/uid/gid/size/atime/mtime 6 个 bool=false）
func emptySattr3() []byte {
	return make([]byte, 24)
}

// Mkdir MKDIR3：diropargs3 + sattr3
func (c *Client) Mkdir(p string) error {
	p = path.Clean("/" + p)
	dirFH, name, err := c.lookupParent(p)
	if err != nil {
		return err
	}
	w := newXDRWriter()
	w.writeOpaque(dirFH)
	w.writeString(name)
	w.buf = append(w.buf, emptySattr3()...)
	_, st, err := c.nfsCall(procMkDir, w.bytes())
	if err != nil {
		return err
	}
	if st != nfs3OK {
		return fmt.Errorf("nfs: mkdir failed, status=%d", st)
	}
	// MKDIR3resok: post_op_fh3 + post_op_attr + wcc_data，无需消费
	c.invalidateCache(p)
	return nil
}

// Remove 根据类型走 REMOVE3 或 RMDIR3；参数均为 diropargs3
func (c *Client) Remove(p string) error {
	p = path.Clean("/" + p)
	isDir := false
	if fi, err := c.Stat(p); err == nil {
		isDir = fi.IsDir
	}
	dirFH, name, err := c.lookupParent(p)
	if err != nil {
		return err
	}
	w := newXDRWriter()
	w.writeOpaque(dirFH)
	w.writeString(name)
	proc := uint32(procRemove)
	if isDir {
		proc = procRmDir
	}
	_, st, err := c.nfsCall(proc, w.bytes())
	if err != nil {
		return err
	}
	if st != nfs3OK {
		return fmt.Errorf("nfs: remove failed, status=%d", st)
	}
	c.invalidateCache(p)
	return nil
}

// Rename RENAME3args { diropargs3 from; diropargs3 to }
func (c *Client) Rename(from, to string) error {
	from = path.Clean("/" + from)
	to = path.Clean("/" + to)
	fromFH, fromName, err := c.lookupParent(from)
	if err != nil {
		return err
	}
	toFH, toName, err := c.lookupParent(to)
	if err != nil {
		return err
	}
	w := newXDRWriter()
	w.writeOpaque(fromFH)
	w.writeString(fromName)
	w.writeOpaque(toFH)
	w.writeString(toName)
	_, st, err := c.nfsCall(procRename, w.bytes())
	if err != nil {
		return err
	}
	if st != nfs3OK {
		return fmt.Errorf("nfs: rename failed, status=%d", st)
	}
	c.invalidateCache(from)
	c.invalidateCache(to)
	return nil
}

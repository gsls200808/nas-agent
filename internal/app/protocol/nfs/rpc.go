package nfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// RPC 程序号
const (
	progPortmap = 100000
	progMountd  = 100005
	progNFS     = 100003
)

// RPC 版本/认证
const (
	rpcVersion  = 2
	authNull    = 0
	authSys     = 1
	protoTCP    = 6
	callMsg     = 0
	replyMsg    = 1
	msgAccepted = 0
)

// xidSeq 全局 XID 递增
var xidSeq uint32

// rpcCall 发起一次 RPC over TCP 调用，返回成功载荷
// prog/vers/proc 为目标程序；args 为已编码的过程参数
func rpcCall(addr string, prog, vers, proc uint32, args []byte) (*xdrReader, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("nfs dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	xidSeq++
	w := newXDRWriter()
	w.writeUint32(xidSeq)
	w.writeUint32(callMsg)
	w.writeUint32(rpcVersion)
	w.writeUint32(prog)
	w.writeUint32(vers)
	w.writeUint32(proc)
	// credential: AUTH_SYS
	cred := buildAuthSys()
	w.writeUint32(authSys)
	w.writeOpaque(cred)
	// verifier: AUTH_NULL
	w.writeUint32(authNull)
	w.writeOpaque(nil)
	w.buf = append(w.buf, args...)

	// record marking：最后一段
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, 0x80000000|uint32(len(w.buf)))
	if _, err := conn.Write(append(hdr, w.buf...)); err != nil {
		return nil, err
	}
	// 读响应（可能分片，本实现一次读完整个记录）
	rhdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, rhdr); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(rhdr)
	if n&0x80000000 == 0 {
		return nil, fmt.Errorf("nfs: fragmented RPC reply unsupported")
	}
	length := int(n & 0x7fffffff)
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	r := newXDRReader(data)
	respXID, _ := r.readUint32()
	_ = respXID
	msgType, err := r.readUint32()
	if err != nil || msgType != replyMsg {
		return nil, fmt.Errorf("nfs: bad reply type %d", msgType)
	}
	replyStat, err := r.readUint32()
	if err != nil || replyStat != msgAccepted {
		return nil, fmt.Errorf("nfs: rpc denied, stat=%d", replyStat)
	}
	// verifier
	if _, err := r.readUint32(); err != nil { // flavor
		return nil, err
	}
	verf, err := r.readOpaque()
	_ = verf
	if err != nil {
		return nil, err
	}
	acceptStat, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	if acceptStat != 0 {
		return nil, fmt.Errorf("nfs: rpc accept stat=%d", acceptStat)
	}
	return r, nil
}

// buildAuthSys 构造 AUTH_SYS 凭证体
// stamp(4) + machinename(string) + uid(4) + gid(4) + gids(array)
func buildAuthSys() []byte {
	w := newXDRWriter()
	w.writeUint32(0xA5A50001) // stamp
	w.writeString("nasagent")
	w.writeUint32(0) // uid
	w.writeUint32(0) // gid
	w.writeUint32(0) // gids count
	return w.bytes()
}

// getport 通过 portmapper 查询程序端口
func getport(portmapAddr string, prog, vers uint32) (uint32, error) {
	w := newXDRWriter()
	w.writeUint32(prog)
	w.writeUint32(vers)
	w.writeUint32(protoTCP)
	w.writeUint32(0)
	r, err := rpcCall(portmapAddr, progPortmap, 2, 3, w.bytes()) // PMAPPROC_GETPORT
	if err != nil {
		return 0, err
	}
	return r.readUint32()
}

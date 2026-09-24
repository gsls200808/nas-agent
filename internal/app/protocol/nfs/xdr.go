package nfs

import (
	"encoding/binary"
	"fmt"
	"math"
)

// XDR (RFC 4506) 编解码：大端序，所有数据按 4 字节对齐填充。

// xdrWriter XDR 写入器
type xdrWriter struct {
	buf []byte
}

func newXDRWriter() *xdrWriter { return &xdrWriter{} }

func (w *xdrWriter) writeUint32(v uint32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	w.buf = append(w.buf, b...)
}

func (w *xdrWriter) writeUint64(v uint64) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	w.buf = append(w.buf, b...)
}

func (w *xdrWriter) writeBool(v bool) {
	if v {
		w.writeUint32(1)
	} else {
		w.writeUint32(0)
	}
}

// writeOpaque 变长不透明数据：length + data + 填充
func (w *xdrWriter) writeOpaque(data []byte) {
	w.writeUint32(uint32(len(data)))
	w.buf = append(w.buf, data...)
	if pad := len(data) % 4; pad != 0 {
		w.buf = append(w.buf, make([]byte, 4-pad)...)
	}
}

// writeFixedOpaque 定长不透明数据（无长度前缀）
func (w *xdrWriter) writeFixedOpaque(data []byte) {
	w.buf = append(w.buf, data...)
	if pad := len(data) % 4; pad != 0 {
		w.buf = append(w.buf, make([]byte, 4-pad)...)
	}
}

func (w *xdrWriter) writeString(s string) { w.writeOpaque([]byte(s)) }

func (w *xdrWriter) bytes() []byte { return w.buf }

// xdrReader XDR 读取器
type xdrReader struct {
	buf []byte
	pos int
}

func newXDRReader(buf []byte) *xdrReader { return &xdrReader{buf: buf} }

func (r *xdrReader) readUint32() (uint32, error) {
	if r.pos+4 > len(r.buf) {
		return 0, ioEOF
	}
	v := binary.BigEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *xdrReader) readUint64() (uint64, error) {
	if r.pos+8 > len(r.buf) {
		return 0, ioEOF
	}
	v := binary.BigEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *xdrReader) readBool() (bool, error) {
	v, err := r.readUint32()
	return v != 0, err
}

// readOpaque 读取变长不透明数据
func (r *xdrReader) readOpaque() ([]byte, error) {
	n, err := r.readUint32()
	if err != nil {
		return nil, err
	}
	if n > math.MaxInt32 || r.pos+int(n) > len(r.buf) {
		return nil, fmt.Errorf("nfs: bad opaque length %d", n)
	}
	data := make([]byte, n)
	copy(data, r.buf[r.pos:r.pos+int(n)])
	r.pos += int(n)
	r.pos += (4 - int(n)%4) % 4
	return data, nil
}

func (r *xdrReader) readString() (string, error) {
	b, err := r.readOpaque()
	return string(b), err
}

// readFixedOpaque 读取定长不透明数据
func (r *xdrReader) readFixedOpaque(n int) ([]byte, error) {
	if r.pos+n > len(r.buf) {
		return nil, ioEOF
	}
	data := make([]byte, n)
	copy(data, r.buf[r.pos:r.pos+n])
	r.pos += n
	r.pos += (4 - n%4) % 4
	return data, nil
}

func (r *xdrReader) remaining() int { return len(r.buf) - r.pos }

var ioEOF = fmt.Errorf("nfs: unexpected end of XDR data")

package smb

import (
	"encoding/binary"
	"time"
)

// utf16LE 将字符串编码为 UTF-16LE
func utf16LE(s string) []byte {
	rs := []rune(s)
	b := make([]byte, len(rs)*2)
	for i, r := range rs {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(r))
	}
	return b
}

// decodeUTF16 将 UTF-16LE 解码为字符串
func decodeUTF16(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	rs := make([]rune, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		rs = append(rs, rune(binary.LittleEndian.Uint16(b[i:])))
	}
	return string(rs)
}

// filetimeNow 当前时间的 Windows FILETIME 值（100ns，自 1601-01-01）
func filetimeNow() int64 {
	return time.Now().UnixNano()/100 + 116444736000000000
}

// filetimeToTime FILETIME 转 time.Time
func filetimeToTime(ft uint64) time.Time {
	return time.Unix(0, int64(ft-116444736000000000)*100)
}

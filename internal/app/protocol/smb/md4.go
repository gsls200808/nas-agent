package smb

// MD4 摘要算法（RFC 1320），Go 标准库不提供，这里纯手写，仅用于 NTLMv2 客户端认证。

import (
	"encoding/binary"
	"math/bits"
)

// MD4 计算 MD4 摘要
func MD4(data []byte) []byte {
	// 初始化链接变量
	a := uint32(0x67452301)
	b := uint32(0xefcdab89)
	c := uint32(0x98badcfe)
	d := uint32(0x10325476)

	// 填充
	msg := make([]byte, len(data))
	copy(msg, data)
	msg = append(msg, 0x80)
	for len(msg)%64 != 56 {
		msg = append(msg, 0)
	}
	bitLen := uint64(len(data)) * 8
	tmp := make([]byte, 8)
	binary.LittleEndian.PutUint64(tmp, bitLen)
	msg = append(msg, tmp...)

	for off := 0; off < len(msg); off += 64 {
		x := make([]uint32, 16)
		for i := 0; i < 16; i++ {
			x[i] = binary.LittleEndian.Uint32(msg[off+i*4:])
		}
		aa, bb, cc, dd := a, b, c, d

		// Round 1
		ff := func(aa, bb, cc, dd *uint32, xk uint32, s int) {
			*aa = bits.RotateLeft32((*aa)+((*bb&(*cc))|(^*bb&(*dd)))+xk, s)
		}
		ff(&a, &b, &c, &d, x[0], 3)
		ff(&d, &a, &b, &c, x[1], 7)
		ff(&c, &d, &a, &b, x[2], 11)
		ff(&b, &c, &d, &a, x[3], 19)
		ff(&a, &b, &c, &d, x[4], 3)
		ff(&d, &a, &b, &c, x[5], 7)
		ff(&c, &d, &a, &b, x[6], 11)
		ff(&b, &c, &d, &a, x[7], 19)
		ff(&a, &b, &c, &d, x[8], 3)
		ff(&d, &a, &b, &c, x[9], 7)
		ff(&c, &d, &a, &b, x[10], 11)
		ff(&b, &c, &d, &a, x[11], 19)
		ff(&a, &b, &c, &d, x[12], 3)
		ff(&d, &a, &b, &c, x[13], 7)
		ff(&c, &d, &a, &b, x[14], 11)
		ff(&b, &c, &d, &a, x[15], 19)

		// Round 2
		gg := func(aa, bb, cc, dd *uint32, xk uint32, s int) {
			*aa = bits.RotateLeft32((*aa)+((*bb&(*cc))|(*bb&(*dd))|(*cc&(*dd)))+xk+0x5a827999, s)
		}
		gg(&a, &b, &c, &d, x[0], 3)
		gg(&d, &a, &b, &c, x[4], 5)
		gg(&c, &d, &a, &b, x[8], 9)
		gg(&b, &c, &d, &a, x[12], 13)
		gg(&a, &b, &c, &d, x[1], 3)
		gg(&d, &a, &b, &c, x[5], 5)
		gg(&c, &d, &a, &b, x[9], 9)
		gg(&b, &c, &d, &a, x[13], 13)
		gg(&a, &b, &c, &d, x[2], 3)
		gg(&d, &a, &b, &c, x[6], 5)
		gg(&c, &d, &a, &b, x[10], 9)
		gg(&b, &c, &d, &a, x[14], 13)
		gg(&a, &b, &c, &d, x[3], 3)
		gg(&d, &a, &b, &c, x[7], 5)
		gg(&c, &d, &a, &b, x[11], 9)
		gg(&b, &c, &d, &a, x[15], 13)

		// Round 3
		hh := func(aa, bb, cc, dd *uint32, xk uint32, s int) {
			*aa = bits.RotateLeft32((*aa)+(*bb^*cc^*dd)+xk+0x6ed9eba1, s)
		}
		hh(&a, &b, &c, &d, x[0], 3)
		hh(&d, &a, &b, &c, x[8], 9)
		hh(&c, &d, &a, &b, x[4], 11)
		hh(&b, &c, &d, &a, x[12], 15)
		hh(&a, &b, &c, &d, x[2], 3)
		hh(&d, &a, &b, &c, x[10], 9)
		hh(&c, &d, &a, &b, x[6], 11)
		hh(&b, &c, &d, &a, x[14], 15)
		hh(&a, &b, &c, &d, x[1], 3)
		hh(&d, &a, &b, &c, x[9], 9)
		hh(&c, &d, &a, &b, x[5], 11)
		hh(&b, &c, &d, &a, x[13], 15)
		hh(&a, &b, &c, &d, x[3], 3)
		hh(&d, &a, &b, &c, x[11], 9)
		hh(&c, &d, &a, &b, x[7], 11)
		hh(&b, &c, &d, &a, x[15], 15)

		a += aa
		b += bb
		c += cc
		d += dd
	}

	out := make([]byte, 16)
	binary.LittleEndian.PutUint32(out[0:], a)
	binary.LittleEndian.PutUint32(out[4:], b)
	binary.LittleEndian.PutUint32(out[8:], c)
	binary.LittleEndian.PutUint32(out[12:], d)
	return out
}

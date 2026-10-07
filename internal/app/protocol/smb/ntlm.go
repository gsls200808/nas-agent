package smb

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
)

// NTLMSSP 客户端实现：构造 Type1/Type3，解析服务端 Type2，完成 NTLMv2 认证。

var ntlmSig = []byte{0x4E, 0x54, 0x4C, 0x4D, 0x53, 0x53, 0x50, 0x00} // "NTLMSSP\0"

var errShort = errors.New("smb: short NTLMSSP message")

// 协商标志
const (
	negUnicode       = 0x00000001
	negRequestTarget = 0x00000004
	negNTLM          = 0x00000200
	negExtendedSec   = 0x00080000
	negTargetInfo    = 0x00800000
	neg128           = 0x20000000
	neg56            = 0x80000000
)

// 注意：不声明 SIGN/SEAL/ALWAYS_SIGN，避免被服务端要求对 SMB2 报文做密钥签名。
func clientFlags() uint32 {
	return negUnicode | negRequestTarget | negNTLM | negExtendedSec | negTargetInfo | neg128 | neg56
}

// type1 构造 NTLMSSP_NEGOTIATE_MESSAGE（domain/workstation 为空）
func type1() []byte {
	const headerLen = 32
	buf := make([]byte, headerLen)
	copy(buf[0:8], ntlmSig)
	binary.LittleEndian.PutUint32(buf[8:12], 1)
	// NegotiateFlags 在偏移 12；DomainNameFields(16) / WorkstationFields(24) 均为 0
	binary.LittleEndian.PutUint32(buf[12:16], clientFlags())
	return buf
}

// type2Info 解析后的 CHALLENGE_MESSAGE 关键信息
type type2Info struct {
	challenge  []byte // 8 字节 ServerChallenge
	targetInfo []byte // AV_PAIR 序列（以 MsvAvEOL 结尾）
}

func parseType2(msg []byte) (*type2Info, error) {
	if len(msg) < 48 {
		return nil, errShort
	}
	info := &type2Info{challenge: make([]byte, 8)}
	copy(info.challenge, msg[24:32])
	tiLen := binary.LittleEndian.Uint16(msg[40:42])
	tiOff := binary.LittleEndian.Uint16(msg[44:48])
	if int(tiOff)+int(tiLen) <= len(msg) {
		info.targetInfo = append([]byte{}, msg[tiOff:tiOff+tiLen]...)
	}
	info.targetInfo = normalizeTargetInfo(info.targetInfo)
	return info, nil
}

// normalizeTargetInfo 规整 AV_PAIR：去掉所有结尾 EOL，再追加一个 EOL
func normalizeTargetInfo(raw []byte) []byte {
	for len(raw) >= 4 {
		id := binary.LittleEndian.Uint16(raw[len(raw)-4:])
		n := binary.LittleEndian.Uint16(raw[len(raw)-2:])
		if id == 0 && n == 0 {
			raw = raw[:len(raw)-4]
			continue
		}
		break
	}
	return append(raw, 0x00, 0x00, 0x00, 0x00)
}

// type3 构造 NTLMSSP_AUTHENTICATE_MESSAGE（NTLMv2 响应）
func type3(username, password, domain, workstation string, info *type2Info) []byte {
	ntlmHash := MD4(utf16LE(password))
	domainUpper := strings.ToUpper(domain)

	// v2hash = HMAC_MD5(ntlmHash, UTF16(uppercase(user)+domain))
	m := hmac.New(md5.New, ntlmHash)
	m.Write(utf16LE(strings.ToUpper(username) + domainUpper))
	v2hash := m.Sum(nil)

	clientChallenge := make([]byte, 8)
	_, _ = rand.Read(clientChallenge)

	// NTLMv2_CLIENT_CHALLENGE blob：
	// RespType(1)=1 HiRespType(1)=1 Reserved(6)=0 TimeStamp(8) ClientChallenge(8) Reserved2(4) TargetInfo...
	blob := make([]byte, 0, 28+len(info.targetInfo))
	blob = append(blob, 0x01, 0x01)
	blob = append(blob, 0, 0, 0, 0, 0, 0)
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, uint64(filetimeNow()))
	blob = append(blob, ts...)
	blob = append(blob, clientChallenge...)
	blob = append(blob, 0, 0, 0, 0)
	blob = append(blob, info.targetInfo...)

	// NTProofStr = HMAC_MD5(v2hash, ServerChallenge || blob)
	mn := hmac.New(md5.New, v2hash)
	mn.Write(info.challenge)
	mn.Write(blob)
	ntProof := mn.Sum(nil)
	ntResp := make([]byte, 0, 16+len(blob))
	ntResp = append(ntResp, ntProof...)
	ntResp = append(ntResp, blob...)

	// LMv2_RESPONSE（24 字节）
	ml := hmac.New(md5.New, v2hash)
	ml.Write(info.challenge)
	ml.Write(clientChallenge)
	lmResp := append(ml.Sum(nil), clientChallenge...)

	dom16 := utf16LE(domainUpper)
	user16 := utf16LE(username)
	ws16 := utf16LE(workstation)

	const headerLen = 64
	type secField struct {
		data []byte
	}
	fields := []secField{{lmResp}, {ntResp}, {dom16}, {user16}, {ws16}, {}}
	off := uint32(headerLen)
	loc := [6]struct{ len, off uint32 }{}
	for i, f := range fields {
		loc[i].len = uint32(len(f.data))
		loc[i].off = off
		off += uint32(len(f.data))
	}
	buf := make([]byte, off)
	copy(buf[0:8], ntlmSig)
	binary.LittleEndian.PutUint32(buf[8:12], 3)
	base := []int{12, 20, 28, 36, 44, 52}
	for i, l := range loc {
		putSecBuf(buf[base[i]:base[i]+8], l.len, l.off)
	}
	binary.LittleEndian.PutUint32(buf[60:64], clientFlags())
	p := uint32(headerLen)
	for _, f := range fields {
		copy(buf[p:], f.data)
		p += uint32(len(f.data))
	}
	return buf
}

// putSecBuf 写入 NTLMSSP 安全缓冲区字段（len 2 + max 2 + offset 4）
func putSecBuf(b []byte, length, offset uint32) {
	binary.LittleEndian.PutUint16(b[0:2], uint16(length))
	binary.LittleEndian.PutUint16(b[2:4], uint16(length))
	binary.LittleEndian.PutUint32(b[4:8], offset)
}

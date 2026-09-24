package smb

// SPNEGO (RFC 4178) GSS 令牌的极简 DER 编解码，用于在 SMB2 SESSION_SETUP 中承载 NTLMSSP。

// spnegoOID SPNEGO: 1.3.6.1.5.5.2
var spnegoOID = []byte{0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}

// ntlmsspOID NTLMSSP: 1.3.6.1.4.1.311.2.2.10
var ntlmsspOID = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a}

// derLen 编码 DER 长度
func derLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

// derWrap TLV 封装
func derWrap(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derLen(len(content))...)
	return append(out, content...)
}

// wrapNegTokenInit 将 NTLMSSP Type1 包装为 SPNEGO NegTokenInit
func wrapNegTokenInit(ntlm []byte) []byte {
	mechList := derWrap(0x30, append(derWrap(0x06, ntlmsspOID)))
	mechTypes := derWrap(0xa0, mechList) // [0] mechTypes
	mechToken := derWrap(0xa2, derWrap(0x04, ntlm)) // [2] mechToken
	negTokenInit := derWrap(0x30, append(mechTypes, mechToken...))
	explicit := derWrap(0xa0, negTokenInit)
	outer := append(derWrap(0x06, spnegoOID), explicit...)
	return derWrap(0x60, outer)
}

// wrapNegTokenResp 将 NTLMSSP Type3 包装为 SPNEGO NegTokenResp
func wrapNegTokenResp(ntlm []byte) []byte {
	respToken := derWrap(0xa2, derWrap(0x04, ntlm)) // [2] responseToken
	negTokenResp := derWrap(0x30, respToken)
	return derWrap(0xa1, negTokenResp)
}

// extractNTLM 从 GSS 令牌（SPNEGO 或裸 NTLMSSP）中切出 NTLMSSP 消息
func extractNTLM(token []byte) []byte {
	for i := 0; i+8 <= len(token); i++ {
		if string(token[i:i+8]) == string(ntlmSig) {
			return token[i:]
		}
	}
	return nil
}

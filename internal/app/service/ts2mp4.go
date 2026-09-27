package service

// 纯 Go 实现的 MPEG-TS → MP4 转封装（remux，流复制，不重新编码）。
// 实现：
//  1. MPEG-TS 解复用：188/192/204 字节包、PAT/PMT、PES（含 PTS/DTS）
//  2. H.264：AnnexB → AVCC（起始码换 4 字节长度前缀），提取 SPS/PPS 构建 avcC
//  3. H.265：AnnexB → HVCC（4 字节长度前缀），提取 VPS/SPS/PPS 构建 hvcC
//  4. AAC：ADTS → 裸帧 + AudioSpecificConfig 构建 esds
//  5. MP4 封装：ftyp / mdat / moov（mvhd、trak、stbl 等）
//
// 仅支持 H.264/H.265 视频 + AAC 音频（HLS 最常见组合），其余流类型会被忽略。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ==================== TS 解复用 ====================

const (
	pidPAT          = 0x0000
	streamTypeH264  = 0x1B
	streamTypeH265  = 0x24
	streamTypeAAC   = 0x0F
	tsPacketPayload = 188
)

// esStream 一路基本流的 PES 组包状态
type esStream struct {
	pid       uint16
	remaining int // 当前 PES 剩余负载字节数，-1 表示未知
	buf       []byte
	pts       int64
	dts       int64
	hasTS     bool
}

// tsDemuxer 将 TS 包解复用，视频/音频帧交给 mp4Muxer
type tsDemuxer struct {
	pmtPID    uint16
	patBuf    []byte
	pmtBuf    []byte
	streams   map[uint16]*esStream
	videoES   *esStream
	audioES   *esStream
	mux       *mp4Muxer
	videoDec  bool
	audioDec  bool
	videoHEVC bool
}

func newTSDemuxer(mux *mp4Muxer) *tsDemuxer {
	return &tsDemuxer{streams: make(map[uint16]*esStream), mux: mux}
}

// feed 处理一个 188 字节的 TS 包（不含 M2TS 前缀）
func (d *tsDemuxer) feed(pkt []byte) {
	if len(pkt) < tsPacketPayload || pkt[0] != 0x47 {
		return
	}
	if pkt[3]&0x80 != 0 { // 加密流，跳过
		return
	}
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	pusi := pkt[1]&0x40 != 0
	afc := pkt[3] >> 4 & 0x03
	payload := pkt[4:]
	if afc == 0 || afc == 2 { // 无负载
		return
	}
	if afc == 3 {
		alen := int(pkt[4])
		if alen > 182 {
			return
		}
		payload = pkt[5+alen:]
	}
	if len(payload) == 0 {
		return
	}
	switch {
	case pid == pidPAT:
		d.feedPSI(&d.patBuf, payload, pusi)
		d.tryParsePAT()
	case d.pmtPID != 0 && pid == d.pmtPID:
		d.feedPSI(&d.pmtBuf, payload, pusi)
		d.tryParsePMT()
	default:
		if es := d.streams[pid]; es != nil {
			d.feedPES(es, payload, pusi)
		}
	}
}

// feedPSI 组包 PSI 段（处理 pointer_field 与跨包）
func (d *tsDemuxer) feedPSI(buf *[]byte, payload []byte, pusi bool) {
	if pusi {
		ptr := int(payload[0])
		if 1+ptr >= len(payload) {
			*buf = nil
			return
		}
		*buf = append((*buf)[:0], payload[1+ptr:]...)
	} else if *buf != nil {
		*buf = append(*buf, payload...)
	}
}

// psiComplete 判断段是否已完整（足够 section_length + CRC）
func psiComplete(t []byte) bool {
	return len(t) >= 8 && len(t) >= 3+int(t[1]&0x0F)<<8+int(t[2])
}

func (d *tsDemuxer) tryParsePAT() {
	if d.patBuf == nil || !psiComplete(d.patBuf) {
		return
	}
	d.parsePAT(d.patBuf)
	d.patBuf = nil
}

func (d *tsDemuxer) tryParsePMT() {
	if d.pmtBuf == nil || !psiComplete(d.pmtBuf) {
		return
	}
	d.parsePMT(d.pmtBuf)
	d.pmtBuf = nil
}

// parsePAT 解析 PAT，取第一个节目的 PMT PID
func (d *tsDemuxer) parsePAT(t []byte) {
	if len(t) < 12 || t[0] != 0x00 {
		return
	}
	secLen := int(t[1]&0x0F)<<8 | int(t[2])
	end := 3 + secLen - 4 // 去掉 CRC32
	if end > len(t) {
		end = len(t)
	}
	for i := 8; i+4 <= end; i += 4 {
		prog := int(t[i])<<8 | int(t[i+1])
		pid := uint16(t[i+2]&0x1F)<<8 | uint16(t[i+3])
		if prog != 0 {
			if d.pmtPID == 0 {
				d.pmtPID = pid
			}
			return
		}
	}
}

// parsePMT 解析 PMT，登记第一个 H.264 与第一个 AAC 流
func (d *tsDemuxer) parsePMT(t []byte) {
	if len(t) < 16 || t[0] != 0x02 {
		return
	}
	secLen := int(t[1]&0x0F)<<8 | int(t[2])
	end := 3 + secLen - 4
	if end > len(t) {
		end = len(t)
	}
	progInfoLen := int(t[10]&0x0F)<<8 | int(t[11])
	i := 12 + progInfoLen
	for i+5 <= end {
		st := t[i]
		pid := uint16(t[i+1]&0x1F)<<8 | uint16(t[i+2])
		esLen := int(t[i+3]&0x0F)<<8 | int(t[i+4])
		i += 5 + esLen
		if !d.videoDec && (st == streamTypeH264 || st == streamTypeH265) {
			d.videoES = &esStream{pid: pid, remaining: -1}
			d.streams[pid] = d.videoES
			d.videoDec = true
			d.videoHEVC = st == streamTypeH265
		} else if !d.audioDec && st == streamTypeAAC {
			d.audioES = &esStream{pid: pid, remaining: -1}
			d.streams[pid] = d.audioES
			d.audioDec = true
		}
	}
}

// feedPES 处理一路基本流的 TS 负载，按 PES 组包
func (d *tsDemuxer) feedPES(es *esStream, payload []byte, pusi bool) {
	if pusi {
		if len(es.buf) > 0 {
			d.flushPES(es)
		}
		if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
			es.buf = nil
			return
		}
		pesLen := int(payload[4])<<8 | int(payload[5])
		hdrLen := int(payload[8])
		if 9+hdrLen > len(payload) {
			es.buf = nil
			return
		}
		es.pts, es.dts, es.hasTS = 0, 0, false
		if payload[6]&0xC0 == 0x80 { // '10' marker
			flags := payload[7] >> 6 & 0x03
			opt := payload[9 : 9+hdrLen]
			if flags >= 2 && len(opt) >= 5 {
				es.pts = parseTSField(opt[0:5])
				es.hasTS = true
				if flags == 3 && len(opt) >= 10 {
					es.dts = parseTSField(opt[5:10])
				} else {
					es.dts = es.pts
				}
			}
		}
		data := payload[9+hdrLen:]
		es.buf = make([]byte, 0, len(data)+1024)
		es.buf = append(es.buf, data...)
		if pesLen >= 6 {
			es.remaining = pesLen - 3 - hdrLen
		} else {
			es.remaining = -1
		}
		if es.remaining >= 0 && len(es.buf) >= es.remaining {
			d.flushPES(es)
		}
	} else if es.buf != nil {
		es.buf = append(es.buf, payload...)
		if es.remaining > 0 && len(es.buf) >= es.remaining {
			d.flushPES(es)
		}
	}
}

// flushPES 一个 PES 组包完成，交给对应的流处理器
func (d *tsDemuxer) flushPES(es *esStream) {
	if len(es.buf) > 0 {
		if es.remaining > 0 && len(es.buf) > es.remaining {
			es.buf = es.buf[:es.remaining]
		}
		if es == d.videoES {
			d.mux.onVideoAccessUnit(es.buf, es.dts, es.pts, es.hasTS, d.videoHEVC)
		} else {
			d.mux.onAACData(es.buf, es.pts, es.hasTS)
		}
	}
	es.buf = nil
	es.remaining = -1
}

// parseTSField 解析 5 字节 PTS/DTS 字段
func parseTSField(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 |
		int64(b[1])<<22 |
		int64(b[2]>>1)<<15 |
		int64(b[3])<<7 |
		int64(b[4]>>1)
}

// ==================== AnnexB / NALU 处理 ====================

// splitAnnexB 按 00 00 01 / 00 00 00 01 起始码切分 NALU
func splitAnnexB(data []byte) [][]byte {
	var nalus [][]byte
	n := len(data)
	naluStart := -1
	i := 0
	for i+2 < n {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			sc := i
			if sc > 0 && data[sc-1] == 0 {
				sc--
			}
			if naluStart >= 0 {
				if nalu := trimTrailingZeros(data[naluStart:sc]); len(nalu) > 0 {
					nalus = append(nalus, nalu)
				}
			}
			i += 3
			naluStart = i
		} else {
			i++
		}
	}
	if naluStart >= 0 && naluStart < n {
		if nalu := trimTrailingZeros(data[naluStart:]); len(nalu) > 0 {
			nalus = append(nalus, nalu)
		}
	}
	return nalus
}

func trimTrailingZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// ==================== SPS 解析（取宽高） ====================

type bitReader struct {
	data []byte
	pos  int
}

func (b *bitReader) bit() uint {
	if b.pos >= len(b.data)*8 {
		b.pos++
		return 0
	}
	v := uint(b.data[b.pos/8]>>uint(7-b.pos%8)) & 1
	b.pos++
	return v
}

func (b *bitReader) bits(n int) uint {
	var v uint
	for i := 0; i < n; i++ {
		v = v<<1 | b.bit()
	}
	return v
}

func (b *bitReader) flag() bool { return b.bit() == 1 }

// ue 无符号 exp-golomb
func (b *bitReader) ue() uint {
	z := 0
	for !b.flag() {
		z++
		if z > 31 {
			return 0
		}
	}
	var suffix uint
	for i := 0; i < z; i++ {
		suffix = suffix<<1 | b.bit()
	}
	return (uint(1)<<uint(z) - 1) + suffix
}

// se 有符号 exp-golomb
func (b *bitReader) se() int {
	k := b.ue()
	if k == 0 {
		return 0
	}
	if k%2 == 1 {
		return int((k + 1) / 2)
	}
	return -int(k / 2)
}

func skipScalingList(b *bitReader, size int) {
	lastScale, nextScale := 8, 8
	for j := 0; j < size; j++ {
		if nextScale != 0 {
			deltaScale := b.se()
			nextScale = (lastScale + deltaScale + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
}

// removeEmulationPrevention 去掉 00 00 03 防竞争字节
func removeEmulationPrevention(nalu []byte) []byte {
	out := make([]byte, 0, len(nalu))
	for i := 0; i < len(nalu); i++ {
		if i+2 < len(nalu) && nalu[i] == 0 && nalu[i+1] == 0 && nalu[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
		} else {
			out = append(out, nalu[i])
		}
	}
	return out
}

// parseSPS 解析 SPS 得到宽高（失败返回 ok=false）
func parseSPS(sps []byte) (w, h int, ok bool) {
	defer func() {
		if recover() != nil {
			w, h, ok = 0, 0, false
		}
	}()
	if len(sps) < 4 {
		return 0, 0, false
	}
	br := &bitReader{data: removeEmulationPrevention(sps)[1:]} // 跳过 NALU 头
	profile := int(br.bits(8))
	br.bits(8) // 约束标记 + 保留位
	br.bits(8) // level_idc
	br.ue()    // sps_id
	chromaFormatIDC := 1
	separateColourPlane := false
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chromaFormatIDC = int(br.ue())
		if chromaFormatIDC == 3 {
			separateColourPlane = br.flag()
		}
		br.ue() // bit_depth_luma_minus8
		br.ue() // bit_depth_chroma_minus8
		br.flag()
		if br.flag() { // seq_scaling_matrix_present_flag
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if br.flag() {
					if i < 6 {
						skipScalingList(br, 16)
					} else {
						skipScalingList(br, 64)
					}
				}
			}
		}
	}
	br.ue() // log2_max_frame_num_minus4
	if pocType := br.ue(); pocType == 0 {
		br.ue()
	} else if pocType == 1 {
		br.flag()
		br.ue()
		br.ue()
		br.ue()
		cnt := br.ue()
		for i := uint(0); i < cnt; i++ {
			br.se()
		}
	}
	br.ue()   // max_num_ref_frames
	br.flag() // gaps_in_frame_num_value_allowed
	widthMbs := int(br.ue()) + 1
	heightMapUnits := int(br.ue()) + 1
	frameMbsOnly := br.flag()
	if !frameMbsOnly {
		br.flag()
	}
	br.flag() // direct_8x8_inference
	cl, cr, ct, cb := 0, 0, 0, 0
	if br.flag() { // frame_cropping
		cl = int(br.ue())
		cr = int(br.ue())
		ct = int(br.ue())
		cb = int(br.ue())
	}
	subW, subH := 1, 1
	switch chromaFormatIDC {
	case 1:
		subW, subH = 2, 2
	case 2:
		subW, subH = 2, 1
	}
	f2 := 2
	if frameMbsOnly {
		f2 = 1
	}
	cropUnitX, cropUnitY := subW, subH*f2
	if separateColourPlane || chromaFormatIDC == 0 {
		cropUnitX, cropUnitY = 1, f2
	}
	width := widthMbs*16 - (cl+cr)*cropUnitX
	height := heightMapUnits*16*f2 - (ct+cb)*cropUnitY
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, false
	}
	return width, height, true
}

// parseHEVCSPS 解析 HEVC SPS 得到宽高与 profile_tier_level（构建 hvcC 用）
func parseHEVCSPS(nalu []byte) (cfg hevcConfig, w, h int, ok bool) {
	defer func() {
		if recover() != nil {
			cfg, w, h, ok = hevcConfig{}, 0, 0, false
		}
	}()
	if len(nalu) < 10 {
		return hevcConfig{}, 0, 0, false
	}
	br := &bitReader{data: removeEmulationPrevention(nalu)}
	br.bits(16) // NALU 头（2 字节）
	br.ue()     // sps_video_parameter_set_id
	maxSubLayers := br.ue()
	if maxSubLayers > 0 { // 多子层时的 profile_tier_level 情况复杂，暂不支持
		return hevcConfig{}, 0, 0, false
	}
	br.bit() // sps_temporal_id_nesting_flag
	// profile_tier_level(1, 0)
	cfg.profileByte = byte(br.bits(2)<<6 | br.bits(1)<<5 | br.bits(5))
	for i := range cfg.compat {
		cfg.compat[i] = byte(br.bits(8))
	}
	for i := range cfg.constraint {
		cfg.constraint[i] = byte(br.bits(8))
	}
	cfg.levelIDC = byte(br.bits(8))
	br.ue() // sps_seq_parameter_set_id
	chroma := br.ue()
	if chroma == 3 {
		br.bit() // separate_colour_plane_flag
	}
	cfg.chromaFormat = byte(chroma)
	w = int(br.ue()) // pic_width_in_luma_samples
	h = int(br.ue()) // pic_height_in_luma_samples
	cl, cr, ct, cb := 0, 0, 0, 0
	if br.flag() { // conformance_window_flag
		cl = int(br.ue())
		cr = int(br.ue())
		ct = int(br.ue())
		cb = int(br.ue())
	}
	cfg.bitDepthLuma = byte(br.ue())
	cfg.bitDepthChroma = byte(br.ue())
	subX, subY := 1, 1
	if chroma == 1 {
		subX, subY = 2, 2
	} else if chroma == 2 {
		subX, subY = 2, 1
	}
	width := w - (cl+cr)*subX
	height := h - (ct+cb)*subY
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return hevcConfig{}, 0, 0, false
	}
	cfg.valid = true
	return cfg, width, height, true
}

// ==================== AAC ADTS 处理 ====================

var aacSampleRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func isADTSSync(h []byte) bool {
	return len(h) >= 2 && h[0] == 0xFF && h[1]&0xF6 == 0xF0 // 12 位同步字 + layer=0
}

// ==================== MP4 封装 ====================

// hevcConfig HEVC profile_tier_level 等信息（构建 hvcC 用）
type hevcConfig struct {
	profileByte    byte // profile_space(2) | tier_flag(1) | profile_idc(5)
	compat         [4]byte
	constraint     [6]byte
	levelIDC       byte
	chromaFormat   byte
	bitDepthLuma   byte // minus 8
	bitDepthChroma byte // minus 8
	valid          bool
}

type mp4Sample struct {
	offset   uint64 // 文件内绝对偏移
	size     uint32
	duration uint32 // timescale 单位
	cts      int32  // pts-dts
	key      bool
}

type mp4Track struct {
	trackID    uint32
	isAudio    bool
	timescale  uint32
	samples    []mp4Sample
	duration   uint64 // timescale 单位
	defaultDur uint32 // 兜底时长（视频 1/25s，音频 1024）
	lastDur    uint32
	baseUnits  int64 // 时间基准（首个样本的 dts/pts）
	lastUnits  int64
	hasBase    bool
	// 视频
	isHEVC  bool
	spsList [][]byte
	ppsList [][]byte
	vpsList [][]byte
	hv      hevcConfig
	width   int
	height  int
	// 音频
	asc        []byte
	sampleRate uint32
	channels   uint16

	// AAC ADTS 流式状态
	carry   []byte
	nextPTS int64
	hasPTS  bool
}

type mp4Muxer struct {
	w            *os.File
	bw           *bufio.Writer
	pos          int64 // 逻辑写入偏移
	mdatSizeAt   int64 // mdat size 字段位置
	video        *mp4Track
	audio        *mp4Track
	err          error
	videoFallDur int64 // 无时间戳时的兜底帧间隔（90kHz）
	base90       int64 // 全局时间基准（首个时间戳，90kHz）
	hasBase90    bool
}

func newMP4Muxer(f *os.File) *mp4Muxer {
	m := &mp4Muxer{w: f, bw: bufio.NewWriterSize(f, 1<<20), videoFallDur: 3600}
	m.write(buildFtyp())
	m.mdatSizeAt = m.pos
	m.write([]byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}) // size 先占位，结束后回填
	return m
}

func (m *mp4Muxer) write(p []byte) {
	if m.err != nil {
		return
	}
	if _, err := m.bw.Write(p); err != nil {
		m.err = err
		return
	}
	m.pos += int64(len(p))
}

// onVideoAccessUnit 处理一个视频访问单元（一个 PES，AnnexB 格式，H.264 或 H.265）
func (m *mp4Muxer) onVideoAccessUnit(data []byte, dts, pts int64, hasTS, hevc bool) {
	if m.video == nil {
		m.video = &mp4Track{trackID: 1, timescale: 90000, defaultDur: 3600, isHEVC: hevc}
	}
	t := m.video
	if !hasTS {
		// 少数流不带时间戳：按 25fps 兜底
		if !t.hasBase {
			t.baseUnits, t.lastUnits, t.hasBase = 0, 0, true
		} else {
			t.lastUnits += m.videoFallDur
		}
		dts = t.lastUnits
		pts = dts
	}
	var frame []byte
	key := false
	for _, nalu := range splitAnnexB(data) {
		if hevc {
			nt := nalu[0] >> 1 & 0x3F // HEVC NALU 头 2 字节，类型占 6 位
			switch nt {
			case 32: // VPS
				m.addVPS(nalu)
				continue
			case 33: // SPS
				m.addSPS(nalu)
				continue
			case 34: // PPS
				m.addPPS(nalu)
				continue
			case 35, 38: // AUD / filler，转封装时丢弃
				continue
			}
			if nt >= 16 && nt <= 21 { // BLA / IDR / CRA 均可作为随机接入点
				key = true
			}
		} else {
			switch nalu[0] & 0x1F {
			case 7: // SPS
				m.addSPS(nalu)
				continue
			case 8: // PPS
				m.addPPS(nalu)
				continue
			case 9, 12: // AUD / filler，转封装时丢弃
				continue
			case 5: // IDR
				key = true
			}
		}
		var lenPrefix [4]byte
		binary.BigEndian.PutUint32(lenPrefix[:], uint32(len(nalu)))
		frame = append(frame, lenPrefix[:]...)
		frame = append(frame, nalu...)
	}
	if len(frame) > 0 {
		dts = m.normBase90(dts)
		pts = m.normBase90(pts)
		m.appendSample(t, dts, pts-dts, frame, key)
	}
}

func (m *mp4Muxer) addSPS(nalu []byte) {
	if m.video.isHEVC {
		if len(nalu) < 10 {
			return
		}
	} else if len(nalu) < 4 {
		return
	}
	for _, s := range m.video.spsList {
		if bytes.Equal(s, nalu) {
			return
		}
	}
	m.video.spsList = append(m.video.spsList, nalu)
	if m.video.isHEVC {
		if !m.video.hv.valid {
			if cfg, w, h, ok := parseHEVCSPS(nalu); ok {
				m.video.hv = cfg
				m.video.width, m.video.height = w, h
			}
		}
	} else if m.video.width == 0 {
		if w, h, ok := parseSPS(nalu); ok {
			m.video.width, m.video.height = w, h
		}
	}
}

// addVPS 收集 HEVC VPS（写入 hvcC）
func (m *mp4Muxer) addVPS(nalu []byte) {
	for _, v := range m.video.vpsList {
		if bytes.Equal(v, nalu) {
			return
		}
	}
	m.video.vpsList = append(m.video.vpsList, nalu)
}

func (m *mp4Muxer) addPPS(nalu []byte) {
	for _, p := range m.video.ppsList {
		if bytes.Equal(p, nalu) {
			return
		}
	}
	m.video.ppsList = append(m.video.ppsList, nalu)
}

// onAACData 处理音频 PES 负载（连续的 ADTS 帧）
func (m *mp4Muxer) onAACData(data []byte, pts int64, hasPTS bool) {
	if m.audio == nil {
		m.audio = &mp4Track{trackID: 2, isAudio: true, defaultDur: 1024}
	}
	t := m.audio
	t.carry = append(t.carry, data...)
	if hasPTS {
		t.nextPTS, t.hasPTS = pts, true
	}
	for {
		if len(t.carry) < 7 {
			return
		}
		h := t.carry
		if !isADTSSync(h) {
			t.carry = t.carry[1:]
			continue
		}
		frameLen := int(h[3]&0x03)<<11 | int(h[4])<<3 | int(h[5])>>5
		hdrLen := 7
		if h[1]&0x01 == 0 { // 有 CRC
			hdrLen = 9
		}
		if frameLen < hdrLen || frameLen > 10*1024 {
			t.carry = t.carry[1:]
			continue
		}
		if len(t.carry) < frameLen {
			return // 等待更多数据
		}
		profile := int(h[2]>>6) & 0x03
		freqIdx := int(h[2]>>2) & 0x0F
		chanCfg := int(h[2]&0x01)<<2 | int(h[3]>>6)&0x03
		if freqIdx >= len(aacSampleRates) {
			t.carry = t.carry[1:]
			continue
		}
		framePTS := t.nextPTS
		if t.hasPTS {
			t.hasPTS = false // 第一个帧使用 PES 的 PTS，后续帧按 1024 样本外推
		}
		m.appendAACSample(t, t.carry[hdrLen:frameLen], framePTS, profile, freqIdx, chanCfg)
		t.carry = t.carry[frameLen:]
		t.nextPTS = framePTS + 1024*90000/int64(aacSampleRates[freqIdx])
	}
}

// normBase90 以首个时间戳为全局基准做归一化（音视频共用，保证 A/V 同步）
func (m *mp4Muxer) normBase90(ts int64) int64 {
	if !m.hasBase90 {
		m.base90, m.hasBase90 = ts, true
	}
	return ts - m.base90
}

func (m *mp4Muxer) appendAACSample(t *mp4Track, data []byte, pts int64, profile, freqIdx, chanCfg int) {
	sampleRate := aacSampleRates[freqIdx]
	if t.sampleRate == 0 {
		t.timescale = uint32(sampleRate)
		t.sampleRate = uint32(sampleRate)
		t.channels = uint16(chanCfg)
		if chanCfg == 0 {
			t.channels = 2
		}
		// AudioSpecificConfig：objectType(5bit) + freqIdx(4bit) + chanCfg(4bit)
		objType := profile + 1
		t.asc = []byte{
			uint8(objType<<3) | uint8(freqIdx>>1),
			uint8(freqIdx&1)<<7 | uint8(chanCfg)<<3,
		}
	}
	// 90kHz PTS → 采样率时间轴
	units := m.normBase90(pts) * int64(sampleRate) / 90000
	m.appendSample(t, units, 0, data, true)
}

// appendSample 写入样本数据并维护样本表
func (m *mp4Muxer) appendSample(t *mp4Track, units int64, cts int64, data []byte, key bool) {
	if !t.hasBase {
		t.baseUnits, t.lastUnits, t.hasBase = units, units, true
	}
	rel := units - t.baseUnits
	if len(t.samples) > 0 {
		dur := rel - t.lastUnits
		if dur <= 0 || dur > int64(t.timescale)*10 { // 非单调或异常大的间隔，用上一段兜底
			dur = int64(t.lastDur)
			if dur == 0 {
				dur = int64(t.defaultDur)
			}
		}
		t.samples[len(t.samples)-1].duration = uint32(dur)
		t.lastDur = uint32(dur)
	}
	t.samples = append(t.samples, mp4Sample{
		offset: uint64(m.pos),
		size:   uint32(len(data)),
		cts:    int32(cts),
		key:    key,
	})
	t.lastUnits = rel
	m.write(data)
}

// finish 收尾：补齐末样本时长、回填 mdat 大小、写 moov
func (m *mp4Muxer) finish() error {
	if m.err != nil {
		return m.err
	}
	if m.video != nil {
		finalizeTrack(m.video)
		if len(m.video.spsList) == 0 || len(m.video.ppsList) == 0 {
			return errors.New("TS 流中未找到视频 SPS/PPS，无法封装 MP4")
		}
	}
	if m.audio != nil {
		finalizeTrack(m.audio)
	}
	if m.video == nil && m.audio == nil {
		return errors.New("TS 流中未找到可封装的 H.264/H.265/AAC 流（不支持其他编码）")
	}
	moov := m.buildMoov()
	mdatEnd := m.pos // moov 写入前的 pos 即 mdat 数据结束位置（mdat 内容 = 8 字节头 + 样本，不含 moov）
	m.write(moov)
	if m.err != nil {
		return m.err
	}
	if err := m.bw.Flush(); err != nil {
		return err
	}
	// 回填 mdat size（8 字节头 + 全部样本数据，不含末尾的 moov）
	if mdatEnd-m.mdatSizeAt > 0xFFFFFFFF {
		return errors.New("输出文件超过 4GB，超出当前封装能力")
	}
	var sizeBytes [4]byte
	binary.BigEndian.PutUint32(sizeBytes[:], uint32(mdatEnd-m.mdatSizeAt))
	_, err := m.w.WriteAt(sizeBytes[:], m.mdatSizeAt)
	return err
}

func finalizeTrack(t *mp4Track) {
	if len(t.samples) == 0 {
		return
	}
	last := &t.samples[len(t.samples)-1]
	if last.duration == 0 {
		last.duration = t.lastDur
	}
	if last.duration == 0 {
		last.duration = t.defaultDur
	}
	var total uint64
	for _, s := range t.samples {
		total += uint64(s.duration)
	}
	t.duration = total
}

func (m *mp4Muxer) buildMoov() []byte {
	var tracks []*mp4Track
	if m.video != nil {
		tracks = append(tracks, m.video)
	}
	if m.audio != nil {
		tracks = append(tracks, m.audio)
	}
	var maxDurMs uint64
	for _, t := range tracks {
		if d := t.duration * 1000 / uint64(t.timescale); d > maxDurMs {
			maxDurMs = d
		}
	}
	b := &boxBuilder{}
	b.blob(buildMvhd(uint32(maxDurMs), uint32(len(tracks)+1)))
	for _, t := range tracks {
		b.blob(buildTrak(t, uint32(maxDurMs)))
	}
	return box("moov", b.Bytes())
}

// ==================== MP4 box 构建 ====================

type boxBuilder struct {
	bytes.Buffer
}

func (b *boxBuilder) u8(v uint8) { _ = b.WriteByte(v) }
func (b *boxBuilder) u16(v uint16) {
	var t [2]byte
	binary.BigEndian.PutUint16(t[:], v)
	_, _ = b.Write(t[:])
}
func (b *boxBuilder) u24(v uint32) { b.u8(uint8(v >> 16)); b.u16(uint16(v)) }
func (b *boxBuilder) u32(v uint32) {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	_, _ = b.Write(t[:])
}
func (b *boxBuilder) u64(v uint64) {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], v)
	_, _ = b.Write(t[:])
}
func (b *boxBuilder) blob(p []byte)  { _, _ = b.Write(p) }
func (b *boxBuilder) ascii(s string) { _, _ = b.WriteString(s) }

func box(name string, body []byte) []byte {
	b := &boxBuilder{}
	b.u32(uint32(len(body) + 8))
	b.ascii(name)
	b.blob(body)
	return b.Bytes()
}

func fullBox(name string, version uint8, flags uint32, body []byte) []byte {
	b := &boxBuilder{}
	b.u8(version)
	b.u24(flags)
	b.blob(body)
	return box(name, b.Bytes())
}

var unityMatrix = []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func buildFtyp() []byte {
	b := &boxBuilder{}
	b.ascii("isom")
	b.u32(0x200)
	b.ascii("isomiso2avc1mp41")
	b.ascii("hvc1")
	return box("ftyp", b.Bytes())
}

func buildMvhd(durationMs, nextTrackID uint32) []byte {
	b := &boxBuilder{}
	b.u32(0) // creation
	b.u32(0) // modification
	b.u32(1000)
	b.u32(durationMs)
	b.u32(0x00010000) // rate
	b.u16(0x0100)     // volume
	b.u16(0)
	b.u32(0)
	b.u32(0)
	for _, v := range unityMatrix {
		b.u32(v)
	}
	for i := 0; i < 6; i++ {
		b.u32(0)
	}
	b.u32(nextTrackID)
	return fullBox("mvhd", 0, 0, b.Bytes())
}

func buildTkhd(t *mp4Track, durationMs uint32) []byte {
	b := &boxBuilder{}
	b.u32(0)
	b.u32(0)
	b.u32(t.trackID)
	b.u32(0)
	b.u32(durationMs)
	b.u32(0)
	b.u32(0)
	b.u16(0) // layer
	b.u16(0) // alternate group
	if t.isAudio {
		b.u16(0x0100)
	} else {
		b.u16(0)
	}
	b.u16(0)
	for _, v := range unityMatrix {
		b.u32(v)
	}
	if t.isAudio {
		b.u32(0)
		b.u32(0)
	} else {
		b.u32(uint32(t.width) << 16)
		b.u32(uint32(t.height) << 16)
	}
	return fullBox("tkhd", 0, 7, b.Bytes())
}

func buildMdhd(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.u32(0)
	b.u32(0)
	b.u32(t.timescale)
	b.u32(uint32(t.duration))
	b.u16(0x55C4) // lang = und
	b.u16(0)
	return fullBox("mdhd", 0, 0, b.Bytes())
}

func buildHdlr(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.u32(0) // predefined
	if t.isAudio {
		b.ascii("soun")
	} else {
		b.ascii("vide")
	}
	b.u32(0) // reserved ×3
	b.u32(0)
	b.u32(0)
	if t.isAudio {
		b.ascii("SoundHandler\x00")
	} else {
		b.ascii("VideoHandler\x00")
	}
	return fullBox("hdlr", 0, 0, b.Bytes())
}

func buildTrak(t *mp4Track, durationMs uint32) []byte {
	b := &boxBuilder{}
	b.blob(buildTkhd(t, durationMs))
	mdia := &boxBuilder{}
	mdia.blob(buildMdhd(t))
	mdia.blob(buildHdlr(t))
	minf := &boxBuilder{}
	if t.isAudio {
		smhd := &boxBuilder{}
		smhd.u16(0)
		smhd.u16(0)
		minf.blob(fullBox("smhd", 0, 0, smhd.Bytes()))
	} else {
		vmhd := &boxBuilder{}
		vmhd.u16(0)
		vmhd.u16(0)
		vmhd.u16(0)
		vmhd.u16(0)
		minf.blob(fullBox("vmhd", 0, 1, vmhd.Bytes()))
	}
	drefBody := &boxBuilder{}
	drefBody.u32(1) // entry_count
	drefBody.blob(fullBox("url ", 0, 1, nil))
	dinf := &boxBuilder{}
	dinf.blob(fullBox("dref", 0, 0, drefBody.Bytes()))
	minf.blob(box("dinf", dinf.Bytes()))
	minf.blob(buildStbl(t))
	mdia.blob(box("minf", minf.Bytes()))
	b.blob(box("mdia", mdia.Bytes()))
	return box("trak", b.Bytes())
}

func buildStsd(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.u32(1) // entry count
	if t.isAudio {
		b.blob(buildMp4a(t))
	} else {
		b.blob(buildVideoEntry(t))
	}
	return fullBox("stsd", 0, 0, b.Bytes())
}

// buildVideoEntry 构建视频样本入口（H.264 → avc1，H.265 → hvc1）
func buildVideoEntry(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.u32(0)   // reserved 前 4 字节
	b.u16(0)   // reserved 后 2 字节（共 6 字节 reserved）
	b.u16(1)   // data_ref_index
	b.u16(0)
	b.u16(0)
	b.u32(0)
	b.u32(0)
	b.u32(0)
	b.u16(uint16(t.width))
	b.u16(uint16(t.height))
	b.u32(0x00480000) // horiz dpi
	b.u32(0x00480000) // vert dpi
	b.u32(0)
	b.u16(1) // frame count
	for i := 0; i < 32; i++ {
		b.u8(0) // compressorname
	}
	b.u16(0x0018) // depth
	b.u16(0xFFFF)
	if t.isHEVC {
		b.blob(box("hvcC", buildHvcC(t)))
		return box("hvc1", b.Bytes())
	}
	// avcC
	sps := t.spsList[0]
	c := &boxBuilder{}
	c.u8(1) // configurationVersion
	c.u8(sps[1])
	c.u8(sps[2])
	c.u8(sps[3])
	c.u8(0xFF) // lengthSizeMinusOne = 3
	c.u8(uint8(0xE0 | len(t.spsList)))
	for _, s := range t.spsList {
		c.u16(uint16(len(s)))
		c.blob(s)
	}
	c.u8(uint8(len(t.ppsList)))
	for _, p := range t.ppsList {
		c.u16(uint16(len(p)))
		c.blob(p)
	}
	b.blob(box("avcC", c.Bytes()))
	return box("avc1", b.Bytes())
}

// buildHvcC 构建 HEVCDecoderConfigurationRecord（hvcC 内容）
func buildHvcC(t *mp4Track) []byte {
	c := &boxBuilder{}
	c.u8(1) // configurationVersion
	c.u8(t.hv.profileByte)
	c.blob(t.hv.compat[:])
	c.blob(t.hv.constraint[:])
	c.u8(t.hv.levelIDC)
	c.u16(0xF000)                         // reserved + min_spatial_segmentation_idc
	c.u8(0xFC)                            // reserved + parallelismType
	c.u8(0xFC | t.hv.chromaFormat&0x03)   // reserved + chromaFormat
	c.u8(0xF8 | t.hv.bitDepthLuma&0x07)   // reserved + bitDepthLumaMinus8
	c.u8(0xF8 | t.hv.bitDepthChroma&0x07) // reserved + bitDepthChromaMinus8
	c.u16(0)                              // avgFrameRate
	c.u8(0x0F)                            // constantFrameRate=0, numTemporalLayers=1, temporalIdNested=1, lengthSizeMinusOne=3
	numArrays := 2
	if len(t.vpsList) > 0 {
		numArrays++
	}
	c.u8(uint8(numArrays))
	if len(t.vpsList) > 0 {
		c.u8(0x80 | 32) // array_completeness=1 + NAL_unit_type=VPS
		c.u16(uint16(len(t.vpsList)))
		for _, v := range t.vpsList {
			c.u16(uint16(len(v)))
			c.blob(v)
		}
	}
	c.u8(0x80 | 33) // SPS
	c.u16(uint16(len(t.spsList)))
	for _, s := range t.spsList {
		c.u16(uint16(len(s)))
		c.blob(s)
	}
	c.u8(0x80 | 34) // PPS
	c.u16(uint16(len(t.ppsList)))
	for _, p := range t.ppsList {
		c.u16(uint16(len(p)))
		c.blob(p)
	}
	return c.Bytes()
}

func descriptor(tag byte, body []byte) []byte {
	return append([]byte{tag, byte(len(body))}, body...)
}

func buildMp4a(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.u32(0)          // reserved 前 4 字节
	b.u16(0)          // reserved 后 2 字节（共 6 字节 reserved）
	b.u16(1)          // data_ref_index
	b.u32(0)          // reserved 4 字节
	b.u32(0)          // reserved 4 字节（共 8 字节 reserved2）
	b.u16(t.channels)
	b.u16(16)         // sample size
	b.u16(0)          // pre_defined
	b.u16(0)          // reserved
	b.u32(t.sampleRate << 16)
	// esds
	dsi := descriptor(0x05, t.asc)
	dcd := &boxBuilder{}
	dcd.u8(0x40) // objectTypeIndication: MPEG-4 AAC
	dcd.u8(0x15) // streamType: audio
	dcd.u24(0)   // bufferSizeDB
	dcd.u32(0)   // maxBitrate
	dcd.u32(0)   // avgBitrate
	dcd.blob(dsi)
	sl := descriptor(0x06, []byte{0x02})
	es := &boxBuilder{}
	es.u16(uint16(t.trackID))
	es.u8(0)
	es.blob(descriptor(0x04, dcd.Bytes()))
	es.blob(sl)
	b.blob(fullBox("esds", 0, 0, descriptor(0x03, es.Bytes())))
	return box("mp4a", b.Bytes())
}

func buildStbl(t *mp4Track) []byte {
	b := &boxBuilder{}
	b.blob(buildStsd(t))
	// stts
	var deltas []uint32
	var counts []uint32
	for _, s := range t.samples {
		if len(deltas) > 0 && deltas[len(deltas)-1] == s.duration {
			counts[len(counts)-1]++
		} else {
			deltas = append(deltas, s.duration)
			counts = append(counts, 1)
		}
	}
	stts := &boxBuilder{}
	stts.u32(uint32(len(deltas)))
	for i := range deltas {
		stts.u32(counts[i])
		stts.u32(deltas[i])
	}
	b.blob(fullBox("stts", 0, 0, stts.Bytes()))
	// ctts（视频且存在 pts != dts 时）
	if !t.isAudio {
		neg, nonzero := false, false
		for _, s := range t.samples {
			if s.cts != 0 {
				nonzero = true
			}
			if s.cts < 0 {
				neg = true
			}
		}
		if nonzero {
			var vals []int32
			var cnts []uint32
			for _, s := range t.samples {
				if len(vals) > 0 && vals[len(vals)-1] == s.cts {
					cnts[len(cnts)-1]++
				} else {
					vals = append(vals, s.cts)
					cnts = append(cnts, 1)
				}
			}
			var ver uint8
			if neg {
				ver = 1
			}
			ctts := &boxBuilder{}
			ctts.u32(uint32(len(vals)))
			for i := range vals {
				ctts.u32(cnts[i])
				if neg {
					ctts.u32(uint32(int32(vals[i])))
				} else {
					ctts.u32(uint32(vals[i]))
				}
			}
			b.blob(fullBox("ctts", ver, 0, ctts.Bytes()))
		}
		// stss（关键帧表；全部为关键帧时省略）
		allKey := true
		for _, s := range t.samples {
			if !s.key {
				allKey = false
				break
			}
		}
		if !allKey {
			stss := &boxBuilder{}
			var keys []uint32
			for i, s := range t.samples {
				if s.key {
					keys = append(keys, uint32(i+1))
				}
			}
			stss.u32(uint32(len(keys)))
			for _, k := range keys {
				stss.u32(k)
			}
			b.blob(fullBox("stss", 0, 0, stss.Bytes()))
		}
	}
	// stsc：每个样本一个 chunk
	stsc := &boxBuilder{}
	stsc.u32(1)
	stsc.u32(1)
	stsc.u32(1)
	stsc.u32(1)
	b.blob(fullBox("stsc", 0, 0, stsc.Bytes()))
	// stsz
	stsz := &boxBuilder{}
	stsz.u32(0)
	stsz.u32(uint32(len(t.samples)))
	for _, s := range t.samples {
		stsz.u32(s.size)
	}
	b.blob(fullBox("stsz", 0, 0, stsz.Bytes()))
	// stco / co64
	var maxOff uint64
	for _, s := range t.samples {
		if uint64(s.offset) > maxOff {
			maxOff = uint64(s.offset)
		}
	}
	if maxOff > 0xFFFFFFFF {
		co64 := &boxBuilder{}
		co64.u32(uint32(len(t.samples)))
		for _, s := range t.samples {
			co64.u64(s.offset)
		}
		b.blob(fullBox("co64", 0, 0, co64.Bytes()))
	} else {
		stco := &boxBuilder{}
		stco.u32(uint32(len(t.samples)))
		for _, s := range t.samples {
			stco.u32(uint32(s.offset))
		}
		b.blob(fullBox("stco", 0, 0, stco.Bytes()))
	}
	return box("stbl", b.Bytes())
}

// ==================== 入口 ====================

// remuxTSToMP4 将多个 TS 分片文件按序转封装为一个 MP4（纯 Go，无 ffmpeg 依赖）
// 仅做流复制（H.264/H.265/AAC），不重新编码；ctx 取消（暂停/删除）时中止合并
func remuxTSToMP4(ctx context.Context, inputFiles []string, outputMP4 string) (err error) {
	if len(inputFiles) == 0 {
		return errors.New("没有可合并的 TS 分片")
	}
	out, err := os.Create(outputMP4)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %v", err)
	}
	defer func() {
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(outputMP4)
		}
	}()
	mux := newMP4Muxer(out)
	demuxer := newTSDemuxer(mux)
	for _, f := range inputFiles {
		if err := demuxTSFile(ctx, f, demuxer, mux); err != nil {
			return err
		}
		if mux.err != nil {
			return mux.err
		}
	}
	return mux.finish()
}

// tsPacketFormat TS 包格式：物理包长、每个物理包内 188 字节 TS 的起始偏移、前导垃圾字节数
type tsPacketFormat struct {
	stride       int
	payloadStart int
	discard      int
}

func detectTSFormat(data []byte) (tsPacketFormat, error) {
	for _, stride := range []int{188, 192, 204} {
		for ps := 0; ps+188 <= stride; ps++ {
			if ps+2*stride < len(data) &&
				data[ps] == 0x47 && data[ps+stride] == 0x47 && data[ps+2*stride] == 0x47 {
				return tsPacketFormat{stride: stride, payloadStart: ps}, nil
			}
		}
	}
	// 兜底：容忍少量前导垃圾，扫描 188 字节同步
	for g := 1; g+188*2 < len(data) && g < 2048; g++ {
		if data[g] == 0x47 && data[g+188] == 0x47 && data[g+2*188] == 0x47 {
			return tsPacketFormat{stride: 188, payloadStart: 0, discard: g}, nil
		}
	}
	return tsPacketFormat{}, errors.New("无法识别 TS 同步字节（非标准 MPEG-TS 流）")
}

func demuxTSFile(ctx context.Context, path string, d *tsDemuxer, m *mp4Muxer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// 探测包格式（188/192/204 字节及起始偏移）
	probe := make([]byte, 4096)
	n, _ := io.ReadFull(f, probe)
	if n == 0 {
		return nil // 空文件，跳过
	}
	format, err := detectTSFormat(probe[:n])
	if err != nil {
		return fmt.Errorf("分片 %s: %v", filepath.Base(path), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(f, 256*1024)
	if format.discard > 0 {
		if _, err := reader.Discard(format.discard); err != nil {
			return err
		}
	}
	pkt := make([]byte, format.stride)
	var count int
	for {
		if count++; count&0x1FF == 0 && ctx.Err() != nil { // 暂停/删除任务时中止合并
			return errTaskPaused
		}
		if _, err := io.ReadFull(reader, pkt); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // 末尾残包忽略
			}
			return err
		}
		d.feed(pkt[format.payloadStart : format.payloadStart+tsPacketPayload])
		if m.err != nil {
			return m.err
		}
	}
	return nil
}

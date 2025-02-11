package track

import (
	"net"
	"time"

	"github.com/pion/rtp/v2"
	"m7s.live/engine/v4/codec"
	. "m7s.live/engine/v4/common"
	"m7s.live/engine/v4/config"
	"m7s.live/engine/v4/util"
)

type H265 struct {
	Video
}

func NewH265(stream IStream) (vt *H265) {
	vt = &H265{}
	vt.Name = "h265"
	vt.CodecID = codec.CodecID_H265
	vt.SampleRate = 90000
	vt.Stream = stream
	vt.Init(256)
	vt.Poll = time.Millisecond * 20
	vt.DecoderConfiguration.PayloadType = 96
	vt.DecoderConfiguration.Raw = make(NALUSlice, 3)
	if config.Global.RTPReorder {
		vt.orderQueue = make([]*RTPFrame, 20)
	}
	return
}
func (vt *H265) WriteAnnexB(pts uint32, dts uint32, frame AnnexBFrame) {
	vt.Value.PTS = pts
	vt.Value.DTS = dts
	for _, slice := range vt.Video.WriteAnnexB(pts, dts, frame) {
		vt.WriteSlice(slice)
	}
	vt.Flush()
}
func (vt *H265) WriteSlice(slice NALUSlice) {
	switch slice.H265Type() {
	case codec.NAL_UNIT_VPS:
		vt.DecoderConfiguration.Raw[0] = slice[0]
	case codec.NAL_UNIT_SPS:
		vt.DecoderConfiguration.Raw[1] = slice[0]
		vt.SPSInfo, _ = codec.ParseHevcSPS(slice[0])
	case codec.NAL_UNIT_PPS:
		vt.DecoderConfiguration.Raw[2] = slice[0]
		extraData, err := codec.BuildH265SeqHeaderFromVpsSpsPps(vt.DecoderConfiguration.Raw[0], vt.DecoderConfiguration.Raw[1], vt.DecoderConfiguration.Raw[2])
		if err == nil {
			vt.DecoderConfiguration.AVCC = net.Buffers{extraData}
		}
		vt.DecoderConfiguration.FLV = codec.VideoAVCC2FLV(net.Buffers(vt.DecoderConfiguration.AVCC), 0)
		vt.DecoderConfiguration.Seq++
	case
		codec.NAL_UNIT_CODED_SLICE_BLA,
		codec.NAL_UNIT_CODED_SLICE_BLANT,
		codec.NAL_UNIT_CODED_SLICE_BLA_N_LP,
		codec.NAL_UNIT_CODED_SLICE_IDR,
		codec.NAL_UNIT_CODED_SLICE_IDR_N_LP,
		codec.NAL_UNIT_CODED_SLICE_CRA:
		vt.Value.IFrame = true
		fallthrough
	case 0, 1, 2, 3, 4, 5, 6, 7, 9:
		vt.Media.WriteSlice(slice)
	}
}
func (vt *H265) WriteAVCC(ts uint32, frame AVCCFrame) {
	if frame.IsSequence() {
		vt.DecoderConfiguration.Seq++
		vt.DecoderConfiguration.AVCC = net.Buffers{frame}
		if vps, sps, pps, err := codec.ParseVpsSpsPpsFromSeqHeaderWithoutMalloc(frame); err == nil {
			vt.SPSInfo, _ = codec.ParseHevcSPS(frame)
			vt.nalulenSize = int(frame[26]) & 0x03
			vt.DecoderConfiguration.Raw[0] = vps
			vt.DecoderConfiguration.Raw[1] = sps
			vt.DecoderConfiguration.Raw[2] = pps
		}
		vt.DecoderConfiguration.FLV = codec.VideoAVCC2FLV(net.Buffers(vt.DecoderConfiguration.AVCC), 0)
	} else {
		vt.Video.WriteAVCC(ts, frame)
		vt.Value.IFrame = frame.IsIDR()
		vt.Flush()
	}
}

// WriteRTPPack 写入已反序列化的RTP包
func (vt *H265) WriteRTPPack(p *rtp.Packet) {
	for frame := vt.UnmarshalRTPPacket(p); frame != nil; frame = vt.nextRTPFrame() {
		vt.writeRTPFrame(frame)
	}
}

// WriteRTP 写入未反序列化的RTP包
func (vt *H265) WriteRTP(raw []byte) {
	for frame := vt.UnmarshalRTP(raw); frame != nil; frame = vt.nextRTPFrame() {
		vt.writeRTPFrame(frame)
	}
}

func (vt *H265) writeRTPFrame(frame *RTPFrame) {
	// TODO: DONL may need to be parsed if `sprop-max-don-diff` is greater than 0 on the RTP stream.
	var usingDonlField bool
	var buffer = util.Buffer(frame.Payload)
	switch frame.H265Type() {
	case codec.NAL_UNIT_RTP_AP:
		buffer.ReadUint16()
		if usingDonlField {
			buffer.ReadUint16()
		}
		for buffer.CanRead() {
			vt.WriteSlice(NALUSlice{buffer.ReadN(int(buffer.ReadUint16()))})
			if usingDonlField {
				buffer.ReadByte()
			}
		}
	case codec.NAL_UNIT_RTP_FU:
		first3 := buffer.ReadN(3)
		fuHeader := first3[2]
		if usingDonlField {
			buffer.ReadUint16()
		}
		if naluType := fuHeader & 0b00111111; util.Bit1(fuHeader, 0) {
			vt.Value.AppendRaw(NALUSlice{[]byte{first3[0]&0b10000001 | (naluType << 1), first3[1]}})
		}
		lastIndex := len(vt.Value.Raw) - 1
		if lastIndex == -1 {
			return
		}
		vt.Value.Raw[lastIndex].Append(buffer)
		if util.Bit1(fuHeader, 1) {
			complete := vt.Value.Raw[lastIndex]     //拼接完成
			vt.Value.Raw = vt.Value.Raw[:lastIndex] // 缩短一个元素，因为后面的方法会加回去
			vt.WriteSlice(complete)
		}
	}
	vt.Value.AppendRTP(frame)
	if frame.Marker {
		vt.generateTimestamp()
		vt.Flush()
	}
}
func (vt *H265) Flush() {
	if vt.Value.IFrame {
		if vt.IDRing == nil {
			defer vt.Attach()
		}
		vt.Video.ComputeGOP()
	}
	// RTP格式补完
	if vt.Value.RTP == nil && config.Global.EnableRTP {
		var out [][]byte
		for _, nalu := range vt.Value.Raw {
			buffers := util.SplitBuffers(nalu, 1200)
			firstBuffer := NALUSlice(buffers[0])
			if l := len(buffers); l == 1 {
				out = append(out, firstBuffer.Bytes())
			} else {
				naluType := firstBuffer.H265Type()
				firstByte := (byte(codec.NAL_UNIT_RTP_FU) << 1) | (firstBuffer[0][0] & 0b10000001)
				buf := []byte{firstByte, firstBuffer[0][1], (1 << 7) | (byte(naluType) >> 1)}
				for i, sp := range firstBuffer {
					if i == 0 {
						sp = sp[2:]
					}
					buf = append(buf, sp...)
				}
				out = append(out, buf)
				for _, bufs := range buffers[1:] {
					buf := []byte{firstByte, firstBuffer[0][1], byte(naluType) >> 1}
					for _, sp := range bufs {
						buf = append(buf, sp...)
					}
					out = append(out, buf)
				}
				lastBuf := out[len(out)-1]
				lastBuf[2] |= 1 << 6 // set end bit
			}
		}
		vt.PacketizeRTP(out...)
	}
	vt.Video.Flush()
}

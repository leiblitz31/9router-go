package petaniproxy

// Minimal pure-Go FLAC encoder for the captcha solver.
//
// Google's free speech-api/v2 endpoint (used by Python's speech_recognition)
// only accepts FLAC or LINEAR16; FLAC halves the upload size. No solid
// pure-Go FLAC encoder exists in the module ecosystem (eaburns/flac and
// mewkiz/flac are decode-only), so this file implements the smallest valid
// FLAC stream: "fLaC" marker + STREAMINFO + frames with VERBATIM subframes
// (no prediction, no Rice coding — still a legal, lossless FLAC stream).
//
// Layout verified against the mewkiz/flac decoder source:
//   - frame header: 14-bit sync 0x3FFE, reserved 0, fixed-blocksize flag,
//     blocksize/sample-rate/channel/bps codes, UTF-8 coded frame number,
//     16-bit blocksize-1, CRC-8 (poly 0x07, init 0)
//   - subframe header byte 0x02 = verbatim, no wasted bits; samples as
//     16-bit big-endian signed (decodeVerbatim sign-extends bps MSB-first)
//   - frame footer CRC-16 (poly 0x8005 IBM, MSB-first, init 0)

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
)

// flacMaxBlock is the number of samples per FLAC frame block. 4096 keeps
// frames small while staying well under the 65535 block-size limit.
const flacMaxBlock = 4096

// flacBitWriter accumulates bits MSB-first into a byte buffer.
type flacBitWriter struct {
	buf   []byte
	acc   uint64
	nbits uint
}

func (w *flacBitWriter) writeBits(v uint64, n uint) {
	w.acc = (w.acc << n) | (v & (uint64(1)<<n - 1))
	w.nbits += n
	for w.nbits >= 8 {
		w.nbits -= 8
		w.buf = append(w.buf, byte(w.acc>>w.nbits))
		w.acc &= uint64(1)<<w.nbits - 1
	}
}

func (w *flacBitWriter) bytes() []byte {
	if w.nbits > 0 {
		w.buf = append(w.buf, byte(w.acc<<(8-w.nbits)))
		w.acc, w.nbits = 0, 0
	}
	return w.buf
}

// flacCRC8 computes the FLAC frame-header CRC-8 (poly 0x07, MSB-first).
func flacCRC8(data []byte) byte {
	crc := byte(0)
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// flacCRC16 computes the FLAC frame-footer CRC-16 (poly 0x8005 IBM, MSB-first).
func flacCRC16(data []byte) uint16 {
	crc := uint16(0)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// flacUTF8 encodes n with FLAC's UTF-8-like variable-length coding.
func flacUTF8(n uint64) []byte {
	var out []byte
	switch {
	case n < 0x80:
		out = []byte{byte(n)}
	case n < 0x800:
		out = []byte{0xC0 | byte(n>>6), 0x80 | byte(n&0x3F)}
	case n < 0x10000:
		out = []byte{0xE0 | byte(n>>12), 0x80 | byte(n>>6)&0x3F, 0x80 | byte(n)&0x3F}
	case n < 0x200000:
		out = []byte{0xF0 | byte(n>>18), 0x80 | byte(n>>12)&0x3F, 0x80 | byte(n>>6)&0x3F, 0x80 | byte(n)&0x3F}
	case n < 0x4000000:
		out = []byte{0xF8 | byte(n>>24), 0x80 | byte(n>>18)&0x3F, 0x80 | byte(n>>12)&0x3F, 0x80 | byte(n>>6)&0x3F, 0x80 | byte(n)&0x3F}
	case n < 0x80000000:
		out = []byte{0xFC | byte(n>>30), 0x80 | byte(n>>24)&0x3F, 0x80 | byte(n>>18)&0x3F, 0x80 | byte(n>>12)&0x3F, 0x80 | byte(n>>6)&0x3F, 0x80 | byte(n)&0x3F}
	default:
		out = []byte{0xFE, 0x80 | byte(n>>30)&0x3F, 0x80 | byte(n>>24)&0x3F, 0x80 | byte(n>>18)&0x3F, 0x80 | byte(n>>12)&0x3F, 0x80 | byte(n>>6)&0x3F, 0x80 | byte(n)&0x3F}
	}
	return out
}

// flacBlockSizes splits n samples into block sizes; every block has at least
// 16 samples (the FLAC minimum) except n < 16, which is rejected by the caller.
func flacBlockSizes(n int) []int {
	if n <= flacMaxBlock {
		return []int{n}
	}
	full, rem := n/flacMaxBlock, n%flacMaxBlock
	sizes := make([]int, 0, full+1)
	for i := 0; i < full; i++ {
		sizes = append(sizes, flacMaxBlock)
	}
	switch {
	case rem == 0:
		// nothing
	case rem >= 16:
		sizes = append(sizes, rem)
	default:
		sizes[len(sizes)-1] += rem // merge tiny tail into previous block
	}
	return sizes
}

// flacMD5 returns the MD5 of the unencoded audio data (16-bit little-endian,
// channels interleaved) for the STREAMINFO block.
func flacMD5(samples []int16) [16]byte {
	h := md5.New()
	var b [2]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint16(b[:], uint16(s))
		h.Write(b[:])
	}
	var sum [16]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// flacRateCode maps common sample rates to their FLAC frame-header literal
// codes (per the official spec). Unknown rates use code 0b1011 with an
// explicit 16-bit Hz value appended to the header.
func flacRateCode(sampleRate int) (code uint64, explicit bool) {
	switch sampleRate {
	case 88200:
		return 0b0001, false
	case 176400:
		return 0b0010, false
	case 192000:
		return 0b0011, false
	case 8000:
		return 0b0100, false
	case 16000:
		return 0b0101, false
	case 22050:
		return 0b0110, false
	case 44100:
		return 0b0111, false
	case 48000:
		return 0b1000, false
	case 96000:
		return 0b1001, false
	default:
		return 0b1011, true
	}
}

// flacFrame encodes one block of mono 16-bit samples as a FLAC frame.
// The frame header carries explicit block size, sample rate and bit depth
// (rather than "from Streaminfo") for maximum decoder compatibility.
func flacFrame(samples []int16, sampleRate, frameNo, blockSize int) []byte {
	rateCode, rateExplicit := flacRateCode(sampleRate)
	hdr := &flacBitWriter{}
	hdr.writeBits(0x3FFE, 14)  // sync
	hdr.writeBits(0, 1)        // reserved
	hdr.writeBits(0, 1)        // fixed block-size strategy
	hdr.writeBits(0b0111, 4)   // block size: 16-bit (blocksize-1) follows
	hdr.writeBits(rateCode, 4) // sample rate
	hdr.writeBits(0b0000, 4)   // channels: mono, independent
	hdr.writeBits(0b100, 3)    // bits-per-sample: 16-bit
	hdr.writeBits(0, 1)        // reserved
	hdrBytes := hdr.bytes()
	hdrBytes = append(hdrBytes, flacUTF8(uint64(frameNo))...)
	var bs [2]byte
	binary.BigEndian.PutUint16(bs[:], uint16(blockSize-1))
	hdrBytes = append(hdrBytes, bs[:]...)
	if rateExplicit {
		binary.BigEndian.PutUint16(bs[:], uint16(sampleRate))
		hdrBytes = append(hdrBytes, bs[:]...)
	}
	hdrBytes = append(hdrBytes, flacCRC8(hdrBytes))

	frame := append([]byte{}, hdrBytes...)
	frame = append(frame, 0x02) // subframe header: verbatim, no wasted bits
	for _, s := range samples {
		frame = append(frame, byte(s>>8), byte(s)) // 16-bit big-endian signed
	}
	crc := flacCRC16(frame)
	frame = append(frame, byte(crc>>8), byte(crc))
	return frame
}

// encodeFLACMono16 encodes mono 16-bit PCM samples into a complete,
// self-contained FLAC stream.
func encodeFLACMono16(samples []int16, sampleRate int) ([]byte, error) {
	if len(samples) == 0 {
		return nil, errors.New("petaniproxy: flac: no samples to encode")
	}
	if sampleRate <= 0 || sampleRate > 655350 {
		return nil, fmt.Errorf("petaniproxy: flac: invalid sample rate %d", sampleRate)
	}
	if len(samples) < 16 {
		return nil, fmt.Errorf("petaniproxy: flac: only %d samples, need at least 16", len(samples))
	}

	blocks := flacBlockSizes(len(samples))
	minBS, maxBS := blocks[0], blocks[0]
	for _, b := range blocks[1:] {
		if b < minBS {
			minBS = b
		}
		if b > maxBS {
			maxBS = b
		}
	}

	var out bytes.Buffer
	out.WriteString("fLaC")

	// STREAMINFO metadata block: last-block flag (bit 7) + type 0 + 34-byte length.
	si := []byte{0x80, 0x00, 0x00, 0x22}
	body := make([]byte, 34)
	binary.BigEndian.PutUint16(body[0:2], uint16(minBS))
	binary.BigEndian.PutUint16(body[2:4], uint16(maxBS))
	// bytes 4-9: min/max frame size, 0 = unknown.
	packed := uint64(sampleRate)<<44 | uint64(0)<<41 | uint64(15)<<36 | uint64(len(samples))
	binary.BigEndian.PutUint64(body[10:18], packed)
	md5sum := flacMD5(samples)
	copy(body[18:34], md5sum[:])
	out.Write(append(si, body...))

	pos := 0
	for i, size := range blocks {
		out.Write(flacFrame(samples[pos:pos+size], sampleRate, i, size))
		pos += size
	}
	return out.Bytes(), nil
}

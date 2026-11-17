package main

import (
	"embed"
	"encoding/binary"
	"fmt"
	"math"
)

// Voice prompts are 8 kHz mono PCM WAVs spoken by espeak-ng's female Alicia
// voice (about 245 Hz for English, about 270 Hz for Ukrainian), then resampled
// with ffmpeg. espeak-ng treats the semicolon as a pause, so the greeting is
// the two clauses below and does not say the word "semicolon".
//
//	espeak-ng -v en-us+Alicia -s 145 -w /tmp/g.wav "For English, press 1; for Ukrainian, press 2."
//	espeak-ng -v en-us+Alicia -s 145 -w /tmp/e.wav "You selected English. Goodbye."
//	espeak-ng -v uk+Alicia -s 140 -w /tmp/u.wav "Ви обрали українську мову. До побачення."
//	ffmpeg -y -i /tmp/g.wav -ac 1 -ar 8000 -c:a pcm_s16le prompts/greeting.wav
//	ffmpeg -y -i /tmp/e.wav -ac 1 -ar 8000 -c:a pcm_s16le prompts/english.wav
//	ffmpeg -y -i /tmp/u.wav -ac 1 -ar 8000 -c:a pcm_s16le prompts/ukrainian.wav
//
//go:embed prompts/*.wav
var promptFS embed.FS

const (
	promptLead = 800  // 100 ms of silence so the first syllable is not clipped
	promptTail = 1600 // 200 ms of silence before the next state
)

func loadPrompts() (greeting, english, ukrainian []int16, err error) {
	if greeting, err = readPrompt("greeting.wav"); err != nil {
		return nil, nil, nil, err
	}
	if english, err = readPrompt("english.wav"); err != nil {
		return nil, nil, nil, err
	}
	if ukrainian, err = readPrompt("ukrainian.wav"); err != nil {
		return nil, nil, nil, err
	}
	return greeting, english, ukrainian, nil
}

func readPrompt(name string) ([]int16, error) {
	b, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		return nil, err
	}
	pcm, err := loadWAV(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(pcm) < 800 {
		return nil, fmt.Errorf("%s: audio is too short", name)
	}
	return pcm, nil
}

// loadWAV reads 16-bit PCM, downmixes stereo, resamples to 8 kHz, and pads
// the clip with a short lead-in and tail.
func loadWAV(b []byte) ([]int16, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF WAVE file")
	}
	var format, channels, bits uint16
	var rate uint32
	var data []byte
	i := 12
	for i+8 <= len(b) {
		id := string(b[i : i+4])
		sz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		i += 8
		if sz < 0 || i+sz > len(b) {
			return nil, fmt.Errorf("truncated %s chunk", id)
		}
		chunk := b[i : i+sz]
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, fmt.Errorf("short fmt chunk")
			}
			format = binary.LittleEndian.Uint16(chunk[0:2])
			channels = binary.LittleEndian.Uint16(chunk[2:4])
			rate = binary.LittleEndian.Uint32(chunk[4:8])
			bits = binary.LittleEndian.Uint16(chunk[14:16])
		case "data":
			data = chunk
		}
		i += sz
		if sz%2 == 1 {
			i++
		}
	}
	if format != 1 || bits != 16 || (channels != 1 && channels != 2) || len(data) < 2 {
		return nil, fmt.Errorf("need 16-bit PCM (got format %d, %d-bit, %d ch)", format, bits, channels)
	}
	frames := len(data) / 2 / int(channels)
	pcm := make([]int16, frames)
	for n := 0; n < frames; n++ {
		if channels == 1 {
			pcm[n] = int16(binary.LittleEndian.Uint16(data[n*2:]))
			continue
		}
		l := int(int16(binary.LittleEndian.Uint16(data[n*4:])))
		r := int(int16(binary.LittleEndian.Uint16(data[n*4+2:])))
		pcm[n] = int16((l + r) / 2)
	}
	if rate != 8000 {
		pcm = resample8k(pcm, int(rate))
	}
	out := make([]int16, promptLead+len(pcm)+promptTail)
	copy(out[promptLead:], pcm)
	return out, nil
}

// resample8k is a box-filter plus linear interpolation. The committed prompts
// are already 8 kHz; this keeps a hand-dropped espeak WAV usable.
func resample8k(in []int16, from int) []int16 {
	if from <= 0 || len(in) == 0 {
		return in
	}
	src := in
	if from > 8000 {
		win := from / 8000
		if win < 1 {
			win = 1
		}
		sm := make([]int16, len(in))
		for i := range in {
			acc, n := 0, 0
			for k := -win; k <= win; k++ {
				j := i + k
				if j >= 0 && j < len(in) {
					acc += int(in[j])
					n++
				}
			}
			sm[i] = int16(acc / n)
		}
		src = sm
	}
	outN := int(int64(len(src)) * 8000 / int64(from))
	if outN < 1 {
		outN = 1
	}
	out := make([]int16, outN)
	last := len(src) - 1
	for i := 0; i < outN; i++ {
		pos := float64(i) * float64(from) / 8000
		j := int(pos)
		if j >= last {
			out[i] = src[last]
			continue
		}
		frac := pos - float64(j)
		a := float64(src[j])
		b := float64(src[j+1])
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}

// mulawEncode is the standard G.711 μ-law encoder (bias 0x84, clip 32635).
func mulawEncode(sample int16) byte {
	const bias = 0x84
	const clip = 32635
	pcm := int(sample)
	sign := 0
	if pcm < 0 {
		sign = 0x80
		pcm = -pcm
		if pcm < 0 {
			pcm = 32767
		}
	}
	if pcm > clip {
		pcm = clip
	}
	pcm += bias
	exp := 7
	mask := 0x4000
	for exp > 0 && pcm&mask == 0 {
		exp--
		mask >>= 1
	}
	mant := (pcm >> (exp + 3)) & 0x0F
	return ^byte(sign | (exp << 4) | mant)
}

func mulawDecode(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exp := (u >> 4) & 0x07
	mant := u & 0x0F
	sample := ((int(mant) << 3) + 0x84) << exp
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	if sample > 32767 {
		return 32767
	}
	if sample < -32768 {
		return -32768
	}
	return int16(sample)
}

// alawEncode / alawDecode implement G.711 A-law so a caller that offers only
// PCMA can still be answered.
func alawEncode(sample int16) byte {
	pcm := int(sample)
	mask := 0xD5
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 8
		if pcm < 0 {
			pcm = 0
		}
	}
	if pcm > 32767 {
		pcm = 32767
	}
	seg := topBit(pcm|0xFF) - 7
	aval := 0x7F
	if seg < 8 {
		aval = seg << 4
		if seg < 2 {
			aval |= (pcm >> 4) & 0x0F
		} else {
			aval |= (pcm >> (seg + 3)) & 0x0F
		}
	}
	return byte(aval ^ mask)
}

func alawDecode(a byte) int16 {
	a ^= 0x55
	t := int(a & 0x0F)
	seg := int((a & 0x70) >> 4)
	switch seg {
	case 0:
		t = (t << 4) + 8
	case 1:
		t = (t << 5) + 0x108
	default:
		t = ((t << 4) + 0x108) << (seg - 1)
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

func topBit(n int) int {
	if n <= 0 {
		return -1
	}
	b := 0
	for n > 0 {
		n >>= 1
		b++
	}
	return b - 1
}

func goertzel(x []int16, freq float64) float64 {
	coeff := 2 * math.Cos(2*math.Pi*freq/8000)
	var s0, s1, s2 float64
	for _, v := range x {
		s0 = float64(v) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	return s1*s1 + s2*s2 - coeff*s1*s2
}

// dtmfLow / dtmfHigh are the standard dual-tone groups.
var (
	dtmfLow  = []float64{697, 770, 852, 941}
	dtmfHigh = []float64{1209, 1336, 1477, 1633}
	dtmfChar = [4][4]int{
		{1, 2, 3, -1},
		{4, 5, 6, -1},
		{7, 8, 9, -1},
		{-1, 0, -1, -1},
	}
)

// decodeDTMF returns 0-9, or -1 when the block is not a digit.
func decodeDTMF(x []int16) int {
	if len(x) < 80 {
		return -1
	}
	var energy float64
	for _, v := range x {
		energy += float64(v) * float64(v)
	}
	rms := math.Sqrt(energy / float64(len(x)))
	if rms < 500 {
		return -1
	}
	lp := make([]float64, 4)
	hp := make([]float64, 4)
	li, hi := 0, 0
	var all float64
	for i, f := range dtmfLow {
		lp[i] = goertzel(x, f)
		all += lp[i]
		if lp[i] > lp[li] {
			li = i
		}
	}
	for i, f := range dtmfHigh {
		hp[i] = goertzel(x, f)
		all += hp[i]
		if hp[i] > hp[hi] {
			hi = i
		}
	}
	var l2, h2 float64
	for i := 0; i < 4; i++ {
		if i != li && lp[i] > l2 {
			l2 = lp[i]
		}
		if i != hi && hp[i] > h2 {
			h2 = hp[i]
		}
	}
	if lp[li] < l2*8 || hp[hi] < h2*8 {
		return -1
	}
	if lp[li]+hp[hi] < all*0.70 {
		return -1
	}
	if hp[hi] == 0 {
		return -1
	}
	twist := lp[li] / hp[hi]
	if twist < 0.1 || twist > 10 {
		return -1
	}
	return dtmfChar[li][hi]
}

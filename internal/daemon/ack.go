package daemon

import "math"

// ackTone is a short two-note confirmation so the user knows the wake word was
// heard while Luna opens recognition. It is generated locally: no TTS call and
// no extra package asset.
func ackTone() []int16 {
	const rate = 48000
	const first, second = 0.14, 0.20
	total := int(rate * (first + second))
	pcm := make([]int16, total)
	for i := range pcm {
		t := float64(i) / rate
		freq := 660.0
		start := 0.0
		length := first
		if t >= first {
			freq = 880.0
			start = first
			length = second
		}
		pos := (t - start) / length
		// 8 ms fade in/out avoids clicks at the edges of each note.
		env := 1.0
		const fade = 0.008
		if pos*length < fade {
			env = pos * length / fade
		}
		if (1-pos)*length < fade {
			env = (1 - pos) * length / fade
		}
		pcm[i] = int16(0.22 * env * math.Sin(2*math.Pi*freq*t) * 32767)
	}
	return pcm
}

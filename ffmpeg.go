package main

import (
	"math"
	"time"
)

// Image sizing and the per-call timeouts of the ffmpeg layer.
const (
	maxImageEdge       = 1568 // long edge of every image the bot builds
	maxImageTokens     = 1568 // ⌈w/28⌉ × ⌈h/28⌉ visual tokens
	stickerSheetFrames = 6
	stickerSheetEdge   = 512
	probeTimeout       = 10 * time.Second
	sheetTimeout       = 60 * time.Second
	lookupTimeout      = 20 * time.Second
)

// fitImageSize returns the largest size with the aspect of w×h whose long edge
// is at most maxEdge and whose ⌈w/28⌉ × ⌈h/28⌉ is at most maxImageTokens. It
// never upscales. Non-positive input gives 0×0.
func fitImageSize(w, h, maxEdge int) (int, int) {
	if w <= 0 || h <= 0 || maxEdge <= 0 {
		return 0, 0
	}
	long, short := max(w, h), min(w, h)
	for l := min(long, maxEdge); l >= 1; l-- {
		s := max(1, short*l/long)
		if ((l+27)/28)*((s+27)/28) <= maxImageTokens {
			if w >= h {
				return l, s
			}
			return s, l
		}
	}
	return 0, 0 // unreachable: 1×1 is one token
}

// gridFor lays n tiles out as ⌈√n⌉ columns and as many rows as needed.
func gridFor(n int) (cols, rows int) {
	if n <= 1 {
		return 1, 1
	}
	cols = int(math.Ceil(math.Sqrt(float64(n))))
	return cols, (n + cols - 1) / cols
}

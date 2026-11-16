package main

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// imageTokens is the vision token estimate ⌈w/28⌉ × ⌈h/28⌉.
func imageTokens(w, h int) int {
	return ((w + 27) / 28) * ((h + 27) / 28)
}

// assertWithinImageLimits checks the sizing rule every image the bot builds must meet.
func assertWithinImageLimits(t *testing.T, w, h, maxEdge int) {
	t.Helper()
	assert.LessOrEqual(t, max(w, h), maxEdge, "long edge of %dx%d", w, h)
	assert.LessOrEqual(t, imageTokens(w, h), maxImageTokens, "tokens of %dx%d", w, h)
}

func TestFitImageSize(t *testing.T) {
	for _, tc := range []struct {
		w, h, maxEdge int
		want          [2]int // zero means "only the limits and aspect are checked"
	}{
		{1280, 720, 1568, [2]int{1280, 720}},
		{720, 1280, 1568, [2]int{720, 1280}},
		{240, 240, 1568, [2]int{240, 240}},
		{512, 512, 512, [2]int{512, 512}},
		{4000, 3000, 1568, [2]int{}},
		{3840, 2160, 1568, [2]int{}},
		{2880, 3840, 1568, [2]int{}}, // a 4×3 grid of 720×1280 frames
		{1536, 1024, 512, [2]int{}},  // a 3×2 sticker grid
		{8192, 16, 1568, [2]int{}},
	} {
		t.Run(fmt.Sprintf("%dx%d max %d", tc.w, tc.h, tc.maxEdge), func(t *testing.T) {
			w, h := fitImageSize(tc.w, tc.h, tc.maxEdge)
			if tc.want != [2]int{} {
				assert.Equal(t, tc.want, [2]int{w, h})
			}
			assertWithinImageLimits(t, w, h, tc.maxEdge)
			assert.LessOrEqual(t, w, tc.w, "never upscaled")
			assert.LessOrEqual(t, h, tc.h, "never upscaled")
			if min(w, h) >= 100 {
				in, out := float64(tc.w)/float64(tc.h), float64(w)/float64(h)
				assert.InDelta(t, 1, out/in, 0.01, "aspect kept within 1%%: %dx%d", w, h)
			}
			// Largest: one more pixel on the long edge (same aspect) is upscaling or breaks a limit.
			long, short := max(tc.w, tc.h), min(tc.w, tc.h)
			if l := max(w, h) + 1; l <= min(long, tc.maxEdge) {
				assert.Greater(t, imageTokens(l, max(1, short*l/long)), maxImageTokens, "a larger size fits")
			}
		})
	}
}

func TestFitImageSize_Degenerate(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, tc := range [][3]int{{0, 720, 1568}, {720, 0, 1568}, {-1, 5, 1568}, {5, 5, 0}} {
		w, h := fitImageSize(tc[0], tc[1], tc[2])
		assert.Equal(t, [2]int{0, 0}, [2]int{w, h}, "%v", tc)
	}
}

func TestGridFor(t *testing.T) {
	for n, want := range map[int][2]int{12: {4, 3}, 6: {3, 2}, 4: {2, 2}, 9: {3, 3}, 20: {5, 4}, 1: {1, 1}, 2: {2, 1}, 5: {3, 2}} {
		cols, rows := gridFor(n)
		assert.Equal(t, want, [2]int{cols, rows}, "gridFor(%d)", n)
		assert.GreaterOrEqual(t, cols*rows, n)
		assert.Equal(t, int(math.Ceil(math.Sqrt(float64(n)))), cols)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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

// Limits on every ffprobe/ffmpeg run. They implement the binding Task 2
// section of docs/superpowers/research/2026-11-16-untrusted-video-parsing-decisions.md
// ("item N" below refers to it).
const (
	toolMaxPixels   = 16777216 // -max_pixels on both tools (item 5)
	probeMaxSide    = 8192     // the probe refuses a wider or taller stream (item 5)
	probeMaxArea    = 16777216 // ... or one with more pixels (item 5)
	toolMaxStreams  = 32       // -max_streams on both tools (item 6)
	toolThreads     = 2        // -threads as an input option (item 6)
	toolOutputLimit = 8 << 20  // stdout bytes a run may write; one more kills it (item 7)
	toolStderrTail  = 64 << 10 // stderr bytes kept for the log (item 7)
	toolWaitDelay   = 2 * time.Second
)

// renderCodecs are the only decoders a render may open (item 4). It maps only
// the chosen video stream, so it never needs an audio or subtitle decoder.
const renderCodecs = "h264,hevc,vp8,vp9,libvpx,libvpx-vp9,libdav1d,libaom-av1,gif"

// probeCodecs is the research's §6.1 probe list without mjpeg and png, so
// files with cover art are refused (item 4, "Cover art"). ffprobe opens a
// decoder for every stream and exits non-zero when one is not on this list.
const probeCodecs = renderCodecs + "," +
	"aac,aac_fixed,mp3,mp3float,mp2,mp2float,opus,libopus,vorbis,libvorbis,flac,alac,ac3,eac3," +
	"amrnb,amrwb,libopencore_amrnb,libopencore_amrwb,adpcm_ima_qt," +
	"pcm_s8,pcm_u8,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s24be,pcm_s32le,pcm_s32be," +
	"pcm_f32le,pcm_f32be,pcm_f64le,pcm_f64be,pcm_mulaw,pcm_alaw," +
	"mov_text,srt,subrip,ssa,ass,webvtt,text"

// videoCodecs are the codec_name values the chosen stream may have (item 4).
// Native av1 decoding is on neither list; av1 decodes through libdav1d or libaom-av1.
var videoCodecs = []string{"h264", "hevc", "vp8", "vp9", "av1", "gif"}

// alphaDecoders keep a WebM stream's alpha channel; the native vp8/vp9 decoders drop it.
var alphaDecoders = map[string]string{"vp8": "libvpx", "vp9": "libvpx-vp9"}

// containers maps an accepted ffprobe format_name to the demuxer name that
// -format_whitelist and -f take, and to the extension a stored file gets.
var containers = []struct{ formatName, demuxer, ext string }{
	{"mov,mp4,m4a,3gp,3g2,mj2", "mov", "mp4"},
	{"matroska,webm", "matroska", "webm"},
	{"gif", "gif", "gif"},
}

// kindFormats is the -format_whitelist for each watchable kind (item 3);
// "sticker" is the video-sticker row.
var kindFormats = map[string][]string{
	"sticker":    {"matroska"},
	"animation":  {"mov", "gif"},
	"video":      {"mov", "matroska"},
	"video_note": {"mov", "matroska"},
	"document":   {"mov", "matroska", "gif"},
}

// probeOutput asks ffprobe for just the fields probeMedia checks.
var probeOutput = []string{
	"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,duration:" +
		"stream_disposition=attached_pic:stream_tags=alpha_mode:stream_side_data=rotation",
	"-of", "json",
}

var (
	// errUnsupportedMedia means the file is not something the bot watches, or
	// ffprobe/ffmpeg failed on it (a non-zero exit or a signal).
	errUnsupportedMedia = errors.New("unsupported media")
	// errToolOutputTooLarge means a run wrote more than toolOutputLimit to stdout.
	errToolOutputTooLarge = errors.New("output over the limit")
)

// mediaProbe is what probeMedia learned about a file.
type mediaProbe struct {
	Format        string  // ffprobe format_name; render calls pin -f (and -format_whitelist) to its demuxer
	Codec         string  // codec_name of the chosen stream (first video stream with attached_pic=0)
	Duration      float64 // seconds; 0 when unknown
	Width, Height int     // display size: swapped when the stream is rotated a quarter turn
	HasAlpha      bool    // VP8/VP9 in WebM with alpha (video stickers)
}

// containerFor finds the accepted container for an ffprobe format_name.
func containerFor(formatName string) (demuxer, ext string, ok bool) {
	for _, c := range containers {
		if formatName == c.formatName || strings.HasPrefix(formatName, c.formatName+",") {
			return c.demuxer, c.ext, true
		}
	}
	return "", "", false
}

// extForFormat is the extension a stored file gets from its probed format:
// "mov,…" → "mp4", "matroska,webm" → "webm", "gif" → "gif", else "". It
// never comes from Telegram's file_path or the user's file name (item 2).
func extForFormat(format string) string {
	_, ext, _ := containerFor(format)
	return ext
}

// probeMedia runs ffprobe on path under the allowlists for kind (a watchable
// kind: "video", "animation", "video_note", "document" or "sticker"), then
// checks the result in Go. Anything not watchable, and any ffprobe failure,
// is errUnsupportedMedia; a timeout or cancellation is the context's error.
func probeMedia(ctx context.Context, path, kind string) (mediaProbe, error) {
	formats, ok := kindFormats[kind]
	if !ok {
		return mediaProbe{}, fmt.Errorf("%w: unknown media kind %q", errUnsupportedMedia, kind)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return mediaProbe{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := runTool(ctx, "ffprobe", filepath.Dir(abs), toolArgs(toolRun{
		tool:    "ffprobe",
		formats: strings.Join(formats, ","),
		codecs:  probeCodecs,
		input:   abs,
		output:  probeOutput,
	}))
	if err != nil {
		return mediaProbe{}, decodeFailure(err)
	}
	return parseProbe(out, formats)
}

// probeStream is one stream of ffprobe's JSON output.
type probeStream struct {
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Duration    string `json:"duration"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	Tags struct {
		AlphaMode string `json:"alpha_mode"`
	} `json:"tags"`
	SideData []struct {
		Rotation float64 `json:"rotation"`
	} `json:"side_data_list"`
}

// parseProbe checks ffprobe's JSON against the container list (formats), the
// stream count cap, the chosen-stream codec list and the pixel caps. The
// chosen stream is the first video stream that is not an attached picture;
// attached pictures never count for codec, size or duration.
func parseProbe(out []byte, formats []string) (mediaProbe, error) {
	var doc struct {
		Streams []probeStream `json:"streams"`
		Format  struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return mediaProbe{}, fmt.Errorf("%w: cannot parse ffprobe output: %w", errUnsupportedMedia, err)
	}
	if demuxer, _, ok := containerFor(doc.Format.FormatName); !ok || !slices.Contains(formats, demuxer) {
		return mediaProbe{}, fmt.Errorf("%w: format %q", errUnsupportedMedia, doc.Format.FormatName)
	}
	if len(doc.Streams) >= toolMaxStreams {
		return mediaProbe{}, fmt.Errorf("%w: %d streams (the -max_streams cap)", errUnsupportedMedia, len(doc.Streams))
	}
	i := slices.IndexFunc(doc.Streams, func(s probeStream) bool {
		return s.CodecType == "video" && s.Disposition.AttachedPic == 0
	})
	if i < 0 {
		return mediaProbe{}, fmt.Errorf("%w: no video stream", errUnsupportedMedia)
	}
	s := doc.Streams[i]
	if !slices.Contains(videoCodecs, s.CodecName) {
		return mediaProbe{}, fmt.Errorf("%w: codec %q", errUnsupportedMedia, s.CodecName)
	}
	if s.Width <= 0 || s.Height <= 0 || s.Width > probeMaxSide || s.Height > probeMaxSide || s.Width*s.Height > probeMaxArea {
		return mediaProbe{}, fmt.Errorf("%w: %dx%d is not a usable size", errUnsupportedMedia, s.Width, s.Height)
	}
	duration, err := probeDuration(s.Duration, doc.Format.Duration)
	if err != nil {
		return mediaProbe{}, err
	}
	w, h := s.Width, s.Height
	for _, sd := range s.SideData {
		if math.Mod(math.Abs(math.Round(sd.Rotation)), 180) == 90 {
			w, h = h, w // autorotate turns the frames before any filter sees them
			break
		}
	}
	return mediaProbe{
		Format:   doc.Format.FormatName,
		Codec:    s.CodecName,
		Duration: duration,
		Width:    w,
		Height:   h,
		HasAlpha: s.Tags.AlphaMode == "1" && alphaDecoders[s.CodecName] != "" && strings.HasPrefix(doc.Format.FormatName, "matroska"),
	}, nil
}

// probeDuration takes the chosen stream's duration, else the container's;
// "N/A" or missing is 0 (unknown). A non-finite or negative value is refused.
func probeDuration(values ...string) (float64, error) {
	for _, v := range values {
		if v == "" || v == "N/A" {
			continue
		}
		d, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			return 0, fmt.Errorf("%w: duration %q", errUnsupportedMedia, v)
		}
		if d > 0 {
			return d, nil
		}
	}
	return 0, nil
}

// decodeFailure turns a run that ffprobe/ffmpeg itself failed into
// errUnsupportedMedia. Timeouts, cancellation, the output bound and a missing
// tool stay what they are.
func decodeFailure(err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return fmt.Errorf("%w: %w", errUnsupportedMedia, err)
	}
	return err
}

// toolRun is one ffprobe or ffmpeg invocation for toolArgs.
type toolRun struct {
	tool      string   // "ffprobe" or "ffmpeg"
	formats   string   // -format_whitelist
	demuxer   string   // -f, pinned from the probe (render only)
	codecs    string   // -codec_whitelist
	decoder   string   // input -c:v (alpha inputs only)
	timeLimit int      // -timelimit CPU seconds (ffmpeg only)
	seek      float64  // input -ss (render only)
	window    float64  // input -t decode window; 0 leaves out -ss and -t
	input     string   // absolute path, passed as file:<path>
	output    []string // everything after the input
}

// toolArgs is the one argument builder for every probe, sheet and lookup
// (item 12). Every input option comes before -i (item 1). ffprobe takes
// neither -nostdin nor -timelimit, so only ffmpeg gets them.
func toolArgs(r toolRun) []string {
	ffmpeg := r.tool == "ffmpeg"
	args := []string{"-hide_banner"}
	if ffmpeg {
		args = append(args, "-nostdin")
	}
	args = append(args, "-loglevel", "error")
	if ffmpeg {
		args = append(args, "-timelimit", strconv.Itoa(r.timeLimit), "-filter_threads", "1", "-copyts")
	}
	args = append(args, "-protocol_whitelist", "file", "-format_whitelist", r.formats)
	if r.demuxer != "" {
		args = append(args, "-f", r.demuxer)
	}
	args = append(args, "-codec_whitelist", r.codecs,
		"-max_pixels", strconv.Itoa(toolMaxPixels),
		"-max_streams", strconv.Itoa(toolMaxStreams),
		"-threads", strconv.Itoa(toolThreads))
	if r.decoder != "" {
		args = append(args, "-c:v", r.decoder)
	}
	if r.window > 0 {
		args = append(args, "-ss", formatSeconds(r.seek), "-t", formatSeconds(r.window))
	}
	args = append(args, "-i", "file:"+r.input)
	return append(args, r.output...)
}

// formatSeconds formats a time already checked finite and in range.
func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 6, 64)
}

// lookupTool resolves an ffmpeg tool to an absolute path, refusing one found
// relative to the working directory (exec.ErrDot, or a relative result under
// GODEBUG=execerrdot=0). It runs on every use, with no caching.
func lookupTool(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found: %w", name, err)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s resolves to the relative path %q", name, p)
	}
	return p, nil
}

// toolSlots bounds concurrent ffprobe/ffmpeg processes across the whole bot
// (item 8). Each run takes a slot for itself alone: never across two runs.
var toolSlots = make(chan struct{}, toolConcurrency(runtime.NumCPU()))

// toolConcurrency is max(1, min(4, cpus/2)).
func toolConcurrency(cpus int) int {
	return max(1, min(4, cpus/2))
}

// toolError is a run that ended with a non-zero exit or a signal. Only the
// exit status decides failure (normal files print "Codec (aac) not on
// whitelist"); the stderr tail is kept for the log.
type toolError struct {
	tool   string
	err    error
	stderr string
}

func (e *toolError) Error() string {
	return fmt.Sprintf("%s: %v; stderr: %q", e.tool, e.err, e.stderr)
}

func (e *toolError) Unwrap() error { return e.err }

// runTool runs one ffprobe/ffmpeg process: an absolute binary, a slot from
// toolSlots (waiting counts against ctx), a minimal environment, stdout bounded
// at toolOutputLimit and the last toolStderrTail bytes of stderr. No retries.
func runTool(ctx context.Context, name, dir string, args []string) ([]byte, error) {
	bin, err := lookupTool(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	select {
	case toolSlots <- struct{}{}:
		defer func() { <-toolSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: waiting for a slot: %w", name, ctx.Err())
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := newToolCmd(runCtx, bin, dir, args)
	stdout := &boundedBuffer{limit: toolOutputLimit, onOverflow: cancel}
	stderr := &tailBuffer{max: toolStderrTail}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	switch {
	case stdout.overflow:
		return nil, fmt.Errorf("%s: %w", name, errToolOutputTooLarge)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%s: %w", name, ctx.Err())
	case err != nil:
		return nil, &toolError{tool: name, err: err, stderr: stderr.String()}
	}
	return stdout.buf.Bytes(), nil
}

// newToolCmd builds the child process: its stdin stays nil (the null device),
// its environment holds only PATH (and SYSTEMROOT on Windows), and it runs in
// dir, the directory that holds only the input file (item 9).
func newToolCmd(ctx context.Context, bin, dir string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- fixed binary, generated arguments
	cmd.Dir = dir
	cmd.Env = toolEnv()
	cmd.WaitDelay = toolWaitDelay
	setToolProcAttr(cmd)
	return cmd
}

// toolEnv is the child's whole environment.
func toolEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if runtime.GOOS == "windows" {
		env = append(env, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	return env
}

// boundedBuffer keeps up to limit bytes. The first write past the limit sets
// overflow, calls onOverflow (which kills the process) and fails.
type boundedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflow   bool
	onOverflow func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:room])
		b.overflow = true
		b.onOverflow()
		return room, errToolOutputTooLarge
	}
	return b.buf.Write(p)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[over:])]
	}
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

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

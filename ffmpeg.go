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
	"sync"
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
	if !usableSize(s.Width, s.Height) {
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

// Bounds on a sheet request. Numbers outside them never reach ffmpeg (item 11).
const (
	maxSheetFrames    = 20                         // the config's largest sheet_frames and max_frames_per_lookup
	minSheetEdge      = 64                         // smallest MaxEdge
	maxSheetSeconds   = maxVideoMaxDurationSeconds // no watchable video is longer
	maxSheetRate      = 1000.0                     // frames per second of source time a sheet may sample
	singleFrameWindow = 1.0                        // seconds decoded after the seek for one frame (Ruling 9)
)

// errInvalidSheet means renderSheet got numbers out of range, or a probe that
// probeMedia would not have produced. Nothing ran.
var errInvalidSheet = errors.New("invalid sheet request")

// sheetSpec asks renderSheet for frames of a probed file.
type sheetSpec struct {
	Start, End float64 // seconds; End ignored when Frames == 1
	Frames     int
	MaxEdge    int    // maxImageEdge, or stickerSheetEdge for stickers
	FontFile   string // the configured font; resolveFontFile picks a built-in one when unusable or ""
}

// sheetPlan is a checked sheet request: every number in it is finite and in range.
type sheetPlan struct {
	demuxer, decoder string    // -f and -format_whitelist; input -c:v for alpha ("" otherwise)
	seek, window     float64   // input -ss and -t (the decode window, Ruling 9)
	rate             float64   // frames per second sampled; 0 for one frame or an unknown duration
	frames           int       // tiles in the image
	cols, rows       int       // grid
	tileW, tileH     int       // even, so tiles line up on chroma rows
	times            []float64 // source time of each tile
}

// renderSheet renders frames of path as one JPEG and returns it with each
// tile's source time. Frames > 1 gives a gridFor(Frames) sheet of frames at
// t_i = Start + i·(End−Start)/Frames; Frames == 1 gives the frame at Start.
// When probe.Duration is 0 (unknown) every frame is the first one, at t=0.
// The image meets the sizing rule for spec.MaxEdge and each tile has its
// source time burned in. It runs one ffmpeg process within sheetTimeout or
// ctx's earlier deadline. A failed run is errUnsupportedMedia; out-of-range
// input is errInvalidSheet; a timeout is the context's error.
func renderSheet(ctx context.Context, path string, probe mediaProbe, spec sheetSpec) (jpeg []byte, frameTimes []float64, err error) {
	plan, err := planSheet(probe, spec)
	if err != nil {
		return nil, nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, sheetTimeout)
	defer cancel()
	out, err := runTool(ctx, "ffmpeg", filepath.Dir(abs), renderArgs(abs, plan, resolveFontFile(spec.FontFile), cpuTimeLimit(ctx)))
	if err != nil {
		return nil, nil, decodeFailure(err)
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%w: ffmpeg wrote no frame", errUnsupportedMedia)
	}
	return out, plan.times, nil
}

// planSheet checks a sheet request and works out every number the render uses.
func planSheet(probe mediaProbe, spec sheetSpec) (sheetPlan, error) {
	if err := checkSheet(probe, spec); err != nil {
		return sheetPlan{}, err
	}
	demuxer, _, _ := containerFor(probe.Format)
	p := sheetPlan{demuxer: demuxer, window: singleFrameWindow, frames: spec.Frames, times: make([]float64, spec.Frames)}
	if probe.HasAlpha {
		p.decoder = alphaDecoders[probe.Codec]
	}
	if probe.Duration > 0 {
		if err := p.sample(min(spec.Start, probe.Duration), min(spec.End, probe.Duration)); err != nil {
			return sheetPlan{}, err
		}
	}
	p.cols, p.rows = gridFor(spec.Frames)
	gridW, gridH := fitImageSize(p.cols*probe.Width, p.rows*probe.Height, spec.MaxEdge)
	p.tileW, p.tileH = max(2, gridW/p.cols&^1), max(2, gridH/p.rows&^1)
	return p, nil
}

// sample sets the seek, decode window, rate and tile times for a file of
// known duration, with start and end already clamped to it.
func (p *sheetPlan) sample(start, end float64) error {
	p.seek, p.times[0] = start, start
	if p.frames == 1 {
		return nil
	}
	span := end - start
	if span <= 0 || float64(p.frames)/span > maxSheetRate {
		return fmt.Errorf("%w: %d frames from %v to %v", errInvalidSheet, p.frames, start, end)
	}
	step := span / float64(p.frames)
	p.rate, p.window = float64(p.frames)/span, span+step
	for i := range p.times {
		p.times[i] = start + float64(i)*step
	}
	return nil
}

// checkSheet refuses a probe probeMedia would not have produced and any
// number out of range, before anything is formatted.
func checkSheet(probe mediaProbe, spec sheetSpec) error {
	_, _, ok := containerFor(probe.Format)
	var bad string
	switch {
	case !ok:
		bad = fmt.Sprintf("format %q", probe.Format)
	case !slices.Contains(videoCodecs, probe.Codec):
		bad = fmt.Sprintf("codec %q", probe.Codec)
	case probe.HasAlpha && alphaDecoders[probe.Codec] == "":
		bad = fmt.Sprintf("alpha on %q", probe.Codec)
	case !usableSize(probe.Width, probe.Height):
		bad = fmt.Sprintf("size %dx%d", probe.Width, probe.Height)
	case !finiteIn(probe.Duration, 0, maxSheetSeconds):
		bad = fmt.Sprintf("duration %v", probe.Duration)
	case !finiteIn(spec.Start, 0, maxSheetSeconds) || !finiteIn(spec.End, 0, maxSheetSeconds):
		bad = fmt.Sprintf("range %v to %v", spec.Start, spec.End)
	case spec.Frames < 1 || spec.Frames > maxSheetFrames:
		bad = fmt.Sprintf("%d frames", spec.Frames)
	case spec.MaxEdge < minSheetEdge || spec.MaxEdge > maxImageEdge:
		bad = fmt.Sprintf("max edge %d", spec.MaxEdge)
	}
	if bad != "" {
		return fmt.Errorf("%w: %s", errInvalidSheet, bad)
	}
	return nil
}

// usableSize is the probe-side pixel cap (item 5).
func usableSize(w, h int) bool {
	return w > 0 && h > 0 && w <= probeMaxSide && h <= probeMaxSide && w*h <= probeMaxArea
}

// finiteIn reports whether v is a finite number in [lo, hi].
func finiteIn(v, lo, hi float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi
}

// renderArgs is the ffmpeg argument vector for a planned sheet. The demuxer is
// pinned with -f and is the only whitelisted format; one JPEG goes to stdout.
func renderArgs(input string, p sheetPlan, font string, timeLimit int) []string {
	return toolArgs(toolRun{
		tool:      "ffmpeg",
		formats:   p.demuxer,
		demuxer:   p.demuxer,
		codecs:    renderCodecs,
		decoder:   p.decoder,
		timeLimit: timeLimit,
		seek:      p.seek,
		window:    p.window,
		input:     input,
		output: []string{"-map", "0:V:0", "-an", "-sn", "-dn", "-vf", sheetFilter(p, font),
			"-frames:v", "1", "-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1"},
	})
}

// alphaOnGray flattens a transparent frame onto mid-gray. Pinning yuva420p
// before split keeps format negotiation from dropping the alpha, which it
// does once -ss or -t is set.
const alphaOnGray = "format=yuva420p,split[fg][bgsrc];[bgsrc]lutyuv=y=128:u=128:v=128:a=255[bg];[bg][fg]overlay=format=yuv420"

// timestampText is drawtext's only text: the frame's time (item 11).
const timestampText = `text='%{pts\:hms}'`

// sheetFilter builds the -vf graph from constants, the plan's checked numbers
// and the escaped font path; nothing in it comes from the file, the user or
// the model (item 11). With -copyts the frames keep their source times.
func sheetFilter(p sheetPlan, font string) string {
	var f []string
	switch {
	case p.rate > 0:
		// Shift the range to start at 0, take one frame every 1/rate s (round=up
		// shows the frame on screen at each tile's time), then shift back in a
		// fine time base so drawtext prints the source time.
		seek := formatSeconds(p.seek)
		f = append(f, "setpts=PTS-"+seek+"/TB",
			"fps=fps="+strconv.FormatFloat(p.rate, 'f', -1, 64)+":start_time=0:round=up",
			"settb=AVTB", "setpts=PTS+"+seek+"/TB")
	case p.frames > 1:
		// Unknown duration: the first frame, repeated, every copy at t=0.
		f = append(f, "trim=end_frame=1", "loop=loop="+strconv.Itoa(p.frames-1)+":size=1", "setpts=0")
	}
	if p.frames > 1 {
		// Exactly the planned frames reach tile, which pads the rest of the grid black at EOF.
		f = append(f, "trim=end_frame="+strconv.Itoa(p.frames))
	}
	if p.decoder != "" {
		f = append(f, alphaOnGray)
	}
	fontSize := min(max(min(p.tileW/12, p.tileH/6), 8), 48)
	border := max(1, fontSize/4)
	draw := "drawtext="
	if font != "" {
		draw += "fontfile=" + filterValue(font) + ":"
	}
	draw += timestampText + fmt.Sprintf(":fontsize=%d:fontcolor=white:box=1:boxcolor=black@0.6:boxborderw=%d:x=%d:y=%d",
		fontSize, border, border, border)
	f = append(f, fmt.Sprintf("scale=%d:%d", p.tileW, p.tileH), draw)
	if p.frames > 1 {
		f = append(f, fmt.Sprintf("tile=%dx%d", p.cols, p.rows))
	}
	return strings.Join(f, ",")
}

// filterValue escapes s as a filter option value (\ : ') and then for the
// filtergraph (\ ' [ ] , ;), the two levels in ffmpeg-filters' "Notes on
// filtergraph escaping". It works on bytes, so any path survives unchanged.
func filterValue(s string) string {
	return escapeBytes(escapeBytes(s, `\:'`), `\'[],;`)
}

func escapeBytes(s, special string) string {
	var b strings.Builder
	for i := range len(s) {
		if strings.IndexByte(special, s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// cpuTimeLimit is ffmpeg's -timelimit: twice the run's wall-clock timeout in
// whole seconds, a CPU-time backstop under the context kill (item 10).
func cpuTimeLimit(ctx context.Context) int {
	d, ok := ctx.Deadline()
	if !ok {
		return 2 * int(sheetTimeout/time.Second)
	}
	return 2 * max(1, int(math.Ceil(time.Until(d).Seconds())))
}

// fontCandidates are the built-in font files, in order (Alpine, Debian/Ubuntu, Windows).
var fontCandidates = []string{
	"/usr/share/fonts/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	`C:\Windows\Fonts\arial.ttf`,
}

// fontFallbacksLogged holds the configured font paths already logged as unusable.
var fontFallbacksLogged sync.Map

// resolveFontFile returns configured when it is an absolute path to a regular
// file. Otherwise (logging once per path when one was configured) it returns
// the first existing built-in candidate, or "" so drawtext uses its default.
func resolveFontFile(configured string) string {
	if configured != "" {
		if usableFontFile(configured) {
			return configured
		}
		if _, seen := fontFallbacksLogged.LoadOrStore(configured, true); !seen {
			InfoLogger.Printf("video: font_file %q is not an absolute path to a regular file; using a built-in font", configured)
		}
	}
	for _, c := range fontCandidates {
		if usableFontFile(c) {
			return c
		}
	}
	return ""
}

func usableFontFile(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	toolMaxAlloc    = 134217728  // -max_alloc on both tools: the largest single allocation (Ruling 12 item 1)
	toolMemoryLimit = 1536 << 20 // address space of each child, on Linux only (Ruling 12 item 2; sandbox_linux.go)
	toolMaxPixels   = 16777216   // -max_pixels on both tools (item 5)
	probeMaxSide    = 8192       // the probe refuses a wider or taller stream (item 5)
	probeMaxArea    = 16777216   // ... or one with more pixels (item 5)
	toolMaxStreams  = 32         // -max_streams on both tools (item 6)
	toolThreads     = 2          // -threads as an input option of a render (item 6)
	probeThreads    = 1          // -threads of a probe: ffprobe opens a decoder for every stream (Ruling 19 item 2)
	encoderThreads  = 1          // -threads as an output option: the mjpeg encoder (see renderArgs)
	toolOutputLimit = 8 << 20    // stdout bytes a run may write; one more kills it (item 7)
	toolStderrTail  = 64 << 10   // stderr bytes kept for the log (item 7)
	toolWaitDelay   = 2 * time.Second
)

// renderCodecs are the only decoders a render may open (item 4). It maps only
// the chosen video stream, so it never needs an audio or subtitle decoder.
// libaom-av1 is left off: like libvpx it checks a frame's size only after
// decoding it, so AV1 decodes through libdav1d, which -max_pixels bounds, or
// not at all (Ruling 12 item 3). libvpx and libvpx-vp9 are not on it either:
// renderArgs adds the one a sticker's alpha plan forces, and nothing else may
// open them, so a build without the native vp8/vp9 decoders cannot fall back
// to libvpx for an ordinary file (Ruling 16 item 5).
const renderCodecs = "h264,hevc,vp8,vp9,libdav1d,gif"

// probeCodecs is the research's §6.1 probe list without mjpeg and png, so
// files with cover art are refused (item 4, "Cover art"), and without libvpx,
// since probing decodes a few frames too. ffprobe opens a decoder for every
// stream and exits non-zero when one is not on this list.
const probeCodecs = renderCodecs + "," +
	"aac,aac_fixed,mp3,mp3float,mp2,mp2float,opus,libopus,vorbis,libvorbis,flac,alac,ac3,eac3," +
	"amrnb,amrwb,libopencore_amrnb,libopencore_amrwb,adpcm_ima_qt," +
	"pcm_s8,pcm_u8,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s24be,pcm_s32le,pcm_s32be," +
	"pcm_f32le,pcm_f32be,pcm_f64le,pcm_f64be,pcm_mulaw,pcm_alaw," +
	"mov_text,srt,subrip,ssa,ass,webvtt,text"

// videoCodecs are the codec_name values the chosen stream may have (item 4).
// Native av1 decoding is on neither list; av1 decodes through libdav1d alone.
var videoCodecs = []string{"h264", "hevc", "vp8", "vp9", "av1", "gif"}

// alphaDecoders keep a WebM stream's alpha channel; the native vp8/vp9 decoders
// drop it. libvpx checks a frame's size only after decoding it, so -max_pixels
// does not bound it: it decodes stickers alone, and only sticker-sized ones
// (Ruling 12 item 3). A size change mid-stream is left to the memory cap.
var alphaDecoders = map[string]string{"vp8": "libvpx", "vp9": "libvpx-vp9"}

// A video sticker must probe at most this large and this long, or it is refused.
const (
	stickerMaxSide    = 512
	stickerMaxSeconds = 10.0
)

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
	"-show_entries", "format=format_name,duration,start_time:stream=codec_type,codec_name,width,height,duration,start_time:" +
		"stream_disposition=attached_pic:stream_tags=alpha_mode,DURATION:stream_side_data=rotation",
	"-of", "json",
}

// matroskaDuration is Matroska's per-stream DURATION tag, e.g. "00:00:04.000000000".
var matroskaDuration = regexp.MustCompile(`^(\d{1,6}):([0-5]\d):([0-5]\d(?:\.\d{1,9})?)$`)

var (
	// errUnsupportedMedia means the file is not something the bot watches:
	// the bot's own checks refused it (its kind, format, codec, stream
	// count, size or duration), or a render ran cleanly and found no frame
	// (errNoFrame).
	errUnsupportedMedia = errors.New("unsupported media")
	// errToolFailed means ffprobe or ffmpeg itself failed: a non-zero exit or
	// a signal. That may be the file (a whitelist refusal, a broken stream)
	// or the run, which a retry can get past: a thread it could not start
	// while other runs held the user's shared task cap (Ruling 19 item 2).
	// The exit status alone cannot tell them apart, and stderr is never
	// parsed.
	errToolFailed = errors.New("the tool failed")
	// errToolOutputTooLarge means a run wrote more than toolOutputLimit to stdout.
	errToolOutputTooLarge = errors.New("output over the limit")
)

// mediaProbe is what probeMedia learned about a file.
type mediaProbe struct {
	Format         string  // ffprobe format_name; render calls pin -f (and -format_whitelist) to its demuxer
	Codec          string  // codec_name of the chosen stream (first video stream with attached_pic=0)
	Duration       float64 // seconds of the chosen stream; 0 when unknown
	Width, Height  int     // display size: swapped when the stream is rotated a quarter turn
	HasAlpha       bool    // VP8/VP9 in WebM with alpha, for kind "sticker" only: decoded with libvpx
	StartTime      float64 // the chosen stream's first timestamp; every sheet time counts from it
	ContainerStart float64 // the file's start time, which ffmpeg's -ss counts from
}

// maxStartSeconds bounds a start time either way; real files are within seconds of 0.
const maxStartSeconds = 24 * 60 * 60

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
// kind: "video", "animation", "video_note", "document" or "sticker"), with
// -f pinned to the demuxer the file's first bytes name (probeDemuxer), then
// checks the result in Go. Anything the checks refuse, the first bytes
// included, is errUnsupportedMedia, an ffprobe failure (a whitelist refusal
// among them) errToolFailed; a timeout or cancellation is the context's
// error.
func probeMedia(ctx context.Context, path, kind string) (mediaProbe, error) {
	formats, ok := kindFormats[kind]
	if !ok {
		return mediaProbe{}, fmt.Errorf("%w: unknown media kind %q", errUnsupportedMedia, kind)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return mediaProbe{}, err
	}
	demuxer, err := probeDemuxer(abs, formats)
	if err != nil {
		return mediaProbe{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := runTool(ctx, "ffprobe", filepath.Dir(abs), "", probeArgs(abs, formats, demuxer))
	if err != nil {
		return mediaProbe{}, toolFailure(err)
	}
	return parseProbe(out, kind)
}

// probeArgs is the ffprobe argument vector for an absolute input path, with
// -f demuxer (probeDemuxer's) unless demuxer is "".
func probeArgs(input string, formats []string, demuxer string) []string {
	return toolArgs(toolRun{
		tool:    "ffprobe",
		formats: strings.Join(formats, ","),
		demuxer: demuxer,
		codecs:  probeCodecs,
		input:   input,
		output:  probeOutput,
	})
}

// probeMagicLen is how much of a file magicDemuxer looks at, and all that
// probeDemuxer reads.
const probeMagicLen = 12

// magicDemuxer is the demuxer the first probeMagicLen bytes of a file name
// (Ruling 20 item 2), or "" for none or a shorter head: an ISO BMFF or
// QuickTime box type at offset 4 is mov, the EBML magic 1A 45 DF A3
// matroska, and GIF87a or GIF89a gif. The box types are those a file may
// start with: ftyp, moov, mdat, free, skip and wide; uuid and junk before
// any of them; legacy QuickTime's pnot and PICT preview; and fragmented
// MP4's moof, styp and sidx (Ruling 21 item 4). All of them pin mov, so no
// other demuxer becomes reachable.
func magicDemuxer(head []byte) string {
	if len(head) < probeMagicLen {
		return ""
	}
	switch {
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "matroska"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif"
	}
	switch string(head[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "uuid", "junk", "pnot", "PICT", "moof", "styp", "sidx":
		return "mov"
	}
	return ""
}

// probeDemuxer is the demuxer ffprobe is pinned to with -f for the file at
// abs, the path probeMedia gives ffprobe: the one its first probeMagicLen
// bytes name (magicDemuxer), which must be on formats, the kind's
// whitelist. Without -f, ffprobe would run every compiled-in demuxer's
// read_probe on the untrusted bytes before the whitelist applies (Ruling 20
// item 2). A file it cannot read that far, or whose bytes name no demuxer
// the kind accepts, is errUnsupportedMedia, and no tool runs.
func probeDemuxer(abs string, formats []string) (string, error) {
	f, err := os.Open(abs) // #nosec G304 -- the bot's own download, the path ffprobe would get
	if err != nil {
		return "", fmt.Errorf("%w: %w", errUnsupportedMedia, err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, probeMagicLen)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", fmt.Errorf("%w: reading its first %d bytes: %w", errUnsupportedMedia, probeMagicLen, err)
	}
	demuxer := magicDemuxer(head)
	switch {
	case demuxer == "":
		return "", fmt.Errorf("%w: its first bytes name no container the bot watches", errUnsupportedMedia)
	case !slices.Contains(formats, demuxer):
		return "", fmt.Errorf("%w: %s is not a container for this kind", errUnsupportedMedia, demuxer)
	}
	return demuxer, nil
}

// probeStream is one stream of ffprobe's JSON output.
type probeStream struct {
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Duration    string `json:"duration"`
	StartTime   string `json:"start_time"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	Tags struct {
		AlphaMode string `json:"alpha_mode"`
		Duration  string `json:"DURATION"`
	} `json:"tags"`
	SideData []struct {
		Rotation float64 `json:"rotation"`
	} `json:"side_data_list"`
}

// parseProbe checks ffprobe's JSON against kind's container list, the stream
// count cap, the chosen-stream codec list and the pixel caps, and a sticker
// against the sticker bounds. The chosen stream is the first video stream
// that is not an attached picture; attached pictures never count for codec,
// size or duration.
func parseProbe(out []byte, kind string) (mediaProbe, error) {
	formats := kindFormats[kind]
	var doc struct {
		Streams []probeStream `json:"streams"`
		Format  struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
			StartTime  string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return mediaProbe{}, fmt.Errorf("%w: cannot parse ffprobe output: %w", errUnsupportedMedia, err)
	}
	demuxer, _, ok := containerFor(doc.Format.FormatName)
	if !ok || !slices.Contains(formats, demuxer) {
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
	sticker := kind == "sticker"
	p := mediaProbe{
		Format:   doc.Format.FormatName,
		Codec:    s.CodecName,
		Width:    s.Width,
		Height:   s.Height,
		HasAlpha: sticker && s.Tags.AlphaMode == "1" && alphaDecoders[s.CodecName] != "" && demuxer == "matroska",
	}
	if err := p.readTimes(s, doc.Format.Duration, doc.Format.StartTime, demuxer == "matroska"); err != nil {
		return mediaProbe{}, err
	}
	for _, sd := range s.SideData {
		if math.Mod(math.Abs(math.Round(sd.Rotation)), 180) == 90 {
			p.Width, p.Height = p.Height, p.Width // autorotate turns the frames before any filter sees them
			break
		}
	}
	if sticker && !stickerSized(p) {
		return mediaProbe{}, fmt.Errorf("%w: a %dx%d, %v s sticker is over %dx%d or %v s",
			errUnsupportedMedia, p.Width, p.Height, p.Duration, stickerMaxSide, stickerMaxSide, stickerMaxSeconds)
	}
	return p, nil
}

// stickerSized reports whether p is within the sticker bounds. An unknown
// duration (0) passes: the render then decodes one fixed window from the start.
func stickerSized(p mediaProbe) bool {
	return p.Width <= stickerMaxSide && p.Height <= stickerMaxSide && p.Duration <= stickerMaxSeconds
}

// readTimes sets the start times and the duration from the chosen stream and
// the container's fields. A start time that is "N/A" or missing is 0.
func (p *mediaProbe) readTimes(s probeStream, formatDuration, formatStart string, matroska bool) error {
	var err error
	if p.StartTime, err = startTime(s.StartTime); err != nil {
		return err
	}
	if p.ContainerStart, err = startTime(formatStart); err != nil {
		return err
	}
	p.Duration, err = probeDuration(s, formatDuration, p.StartTime, matroska)
	return err
}

// startTime parses an ffprobe start_time, which must be finite and within
// maxStartSeconds of 0. "N/A" or missing is 0.
func startTime(v string) (float64, error) {
	if v == "" || v == "N/A" {
		return 0, nil
	}
	t, err := strconv.ParseFloat(v, 64)
	if err != nil || !finiteIn(t, -maxStartSeconds, maxStartSeconds) {
		return 0, fmt.Errorf("%w: start time %q", errUnsupportedMedia, v)
	}
	return t, nil
}

// probeDuration takes the chosen stream's duration, then its Matroska
// DURATION tag (Matroska streams have no duration field), then the
// container's, which also covers longer audio. Matroska's tag and file
// duration say where the stream ends on the file's clock, so its length is
// that minus the stream's start. 0 means unknown. A non-finite or negative
// duration field is refused; a tag that is not a well-formed H:MM:SS time is
// ignored.
func probeDuration(s probeStream, formatDuration string, start float64, matroska bool) (float64, error) {
	if d, err := numericDuration(s.Duration); err != nil || d > 0 {
		return d, err
	}
	length := func(end float64) float64 {
		if !matroska || end == 0 {
			return end
		}
		return max(0, end-start)
	}
	if d, ok := tagDuration(s.Tags.Duration); ok && length(d) > 0 {
		return length(d), nil
	}
	d, err := numericDuration(formatDuration)
	return length(d), err
}

// numericDuration parses one of ffprobe's duration fields; "N/A" or missing is 0.
func numericDuration(v string) (float64, error) {
	if v == "" || v == "N/A" {
		return 0, nil
	}
	d, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
		return 0, fmt.Errorf("%w: duration %q", errUnsupportedMedia, v)
	}
	return d, nil
}

// tagDuration reads a Matroska DURATION tag in seconds: hours, minutes under
// 60 and seconds under 60, so the result is finite and in range by
// construction. ok is false for a missing, malformed or zero tag.
func tagDuration(tag string) (float64, bool) {
	m := matroskaDuration.FindStringSubmatch(tag)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	secs, _ := strconv.ParseFloat(m[3], 64)
	d := float64(h)*3600 + float64(mins)*60 + secs
	return d, d > 0
}

// toolFailure turns a run that ffprobe/ffmpeg itself failed into
// errToolFailed. Timeouts, cancellation, the output bound and a missing tool
// stay what they are.
func toolFailure(err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return fmt.Errorf("%w: %w", errToolFailed, err)
	}
	return err
}

// toolRun is one ffprobe or ffmpeg invocation for toolArgs.
type toolRun struct {
	tool      string   // "ffprobe" or "ffmpeg"
	formats   string   // -format_whitelist
	demuxer   string   // -f: from the first bytes (probe) or the probed format (render)
	codecs    string   // -codec_whitelist
	decoder   string   // input -c:v (alpha inputs only)
	timeLimit int      // -timelimit CPU seconds (ffmpeg only, and only where toolTimeLimitFlag)
	seek      float64  // input -ss (render only)
	window    float64  // input -t decode window; 0 leaves out -ss and -t
	input     string   // absolute path, passed as file:<path>
	output    []string // everything after the input
}

// toolArgs is the one argument builder for every probe, sheet and lookup
// (item 12). Every input option comes before -i (item 1). ffprobe takes
// neither -nostdin nor -timelimit, so only ffmpeg gets them, and -timelimit
// only off Linux, where no helper sets RLIMIT_CPU (toolTimeLimitFlag).
//
// There is no -fflags +noparse+nofillin, which would stop the codec parsers
// running on streams the decoder lists then refuse. It was tried (Ruling 12
// item 4) and broke ffmpeg 6.1.1, 8.1.3 and 9.0.2 alike: every GIF render
// failed (exit -22; +noparse alone does that, seen on 9.0.2), and Matroska
// stream durations and start times were lost.
func toolArgs(r toolRun) []string {
	ffmpeg := r.tool == "ffmpeg"
	threads := probeThreads
	if ffmpeg {
		threads = toolThreads
	}
	args := []string{"-hide_banner"}
	if ffmpeg {
		args = append(args, "-nostdin")
	}
	args = append(args, "-loglevel", "error", "-max_alloc", strconv.Itoa(toolMaxAlloc))
	if ffmpeg && toolTimeLimitFlag {
		args = append(args, "-timelimit", strconv.Itoa(r.timeLimit))
	}
	if ffmpeg {
		args = append(args, "-filter_threads", "1", "-copyts")
	}
	args = append(args, "-protocol_whitelist", "file", "-format_whitelist", r.formats)
	if r.demuxer != "" {
		args = append(args, "-f", r.demuxer)
	}
	args = append(args, "-codec_whitelist", r.codecs,
		"-max_pixels", strconv.Itoa(toolMaxPixels),
		"-max_streams", strconv.Itoa(toolMaxStreams),
		"-threads", strconv.Itoa(threads))
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

// signedSeconds formats a checked time as a term to add: "+1.500000" or "-1.500000".
func signedSeconds(s float64) string {
	if s < 0 {
		return "-" + formatSeconds(-s)
	}
	return "+" + formatSeconds(s)
}

// Bounds on a sheet request. Numbers outside them never reach ffmpeg (item 11).
const (
	maxSheetFrames    = max(maxVideoSheetFrames, maxVideoMaxFramesPerLookup)
	minSheetEdge      = 64                         // smallest MaxEdge
	maxSheetSeconds   = maxVideoMaxDurationSeconds // no watchable video is longer
	maxSheetRate      = 1000.0                     // frames per second of source time a sheet may sample
	singleFrameWindow = 1.0                        // W for one frame: seconds decoded after Start
	lookback          = 10.0                       // L: seconds decoded before Start (Ruling 10)
	endMargin         = 0.001                      // a single frame is taken at least this far before the end
)

var (
	// errInvalidSheet means renderSheet got numbers out of range, or a probe
	// that probeMedia would not have produced. Nothing ran.
	errInvalidSheet = errors.New("invalid sheet request")
	// errNoFrame means ffmpeg exited cleanly but produced no image. Usually
	// nothing was on screen within the lookback before Start; on ffmpeg 6.x a
	// refused decoder or a frame over -max_pixels can end the same way. It is
	// also errUnsupportedMedia.
	errNoFrame = errors.New("no frame near that time")
)

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
	start            float64   // S: tile times count from here
	seek, window     float64   // max(0, S−L) and W+L: the decode window (Ruling 10), relative like S
	origin           float64   // the video's first timestamp: relative times are measured from it
	inputSeek        float64   // input -ss: the window's start on the clock -ss uses (the file's start time)
	rate             float64   // frames per second sampled; 0 for an unknown duration
	frames           int       // tiles in the image
	cols, rows       int       // grid
	tileW, tileH     int       // even, so tiles line up on chroma rows
	times            []float64 // time of each tile, relative to the video's start
}

// renderSheet renders frames of path as one JPEG and returns it with each
// tile's time. Every time it takes or returns counts from the video's first
// frame (probe.StartTime), whatever the file's own clock says. Frames > 1
// gives a gridFor(Frames) sheet of the frames on screen at
// t_i = Start + i·(End−Start)/Frames; Frames == 1 gives the frame on screen at
// Start, and a Start at or past the end gives the last frame, with its time
// just before the end. Decoding starts `lookback` seconds before Start, so a
// frame that began up to that long before a tile's time is found; one that
// began earlier is not (errNoFrame where ffmpeg can tell, else a render
// failure). When probe.Duration is 0 (unknown) every frame is the first one,
// at t=0. The image meets the sizing rule for spec.MaxEdge and each tile has
// its time burned in.
//
// It runs one ffmpeg process, capped at sheetTimeout. It never extends ctx's
// deadline, so the caller passes the per-call one in ctx: a first sheet
// passes its context as is, and a lookup passes one made with
// context.WithTimeout(ctx, lookupTimeout).
//
// A failed run is errToolFailed, a clean one with no frame errNoFrame;
// out-of-range input is errInvalidSheet; a timeout is the context's error.
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
	font := resolveFontFile(spec.FontFile)
	out, err := renderOutcome(runTool(ctx, "ffmpeg", filepath.Dir(abs), font, renderArgs(abs, plan, font, cpuTimeLimit(ctx))))
	if err != nil {
		return nil, nil, err
	}
	return out, plan.times, nil
}

// renderOutcome sorts out a render run. A clean exit that wrote nothing means
// no frame reached the encoder: errNoFrame (on 6.x that can also be a
// refusal). ffmpeg 7 and later exit with −22 when no frame comes, the same
// status as a refused decoder, an unusable font or a thread it could not
// start, so such a run cannot be told apart by its status and stays a plain
// errToolFailed.
func renderOutcome(out []byte, err error) ([]byte, error) {
	switch {
	case err != nil:
		return nil, toolFailure(err)
	case len(out) == 0:
		return nil, fmt.Errorf("%w (%w)", errNoFrame, errUnsupportedMedia)
	}
	return out, nil
}

// planSheet checks a sheet request and works out every number the render uses.
func planSheet(probe mediaProbe, spec sheetSpec) (sheetPlan, error) {
	if err := checkSheet(probe, spec); err != nil {
		return sheetPlan{}, err
	}
	demuxer, _, _ := containerFor(probe.Format)
	p := sheetPlan{demuxer: demuxer, window: singleFrameWindow + lookback, frames: spec.Frames, times: make([]float64, spec.Frames)}
	if probe.HasAlpha {
		p.decoder = alphaDecoders[probe.Codec]
	}
	if probe.Duration > 0 {
		if err := p.sample(spec.Start, spec.End, probe.Duration); err != nil {
			return sheetPlan{}, err
		}
	}
	// With -copyts, -ss counts from the file's start time while frames carry
	// the file's clock; the plan's times count from the video's first frame.
	p.origin = probe.StartTime
	p.inputSeek = max(0, probe.StartTime+p.seek-probe.ContainerStart)
	p.cols, p.rows = gridFor(spec.Frames)
	gridW, gridH := fitImageSize(p.cols*probe.Width, p.rows*probe.Height, spec.MaxEdge)
	p.tileW, p.tileH = max(2, gridW/p.cols&^1), max(2, gridH/p.rows&^1)
	return p, nil
}

// sample sets the tile times, the sampling rate and the decode window for a
// file of known duration. Start and End are clamped to it; a single frame is
// taken at least endMargin before the end, since the last frame stays on
// screen until then and nothing is on screen at the end itself. The window W
// is End−Start plus one step, or singleFrameWindow for one frame; decoding
// starts lookback seconds early (Ruling 10).
func (p *sheetPlan) sample(start, end, duration float64) error {
	start, end = min(start, duration), min(end, duration)
	width := singleFrameWindow
	p.rate = 1 / singleFrameWindow
	if p.frames == 1 {
		start = min(start, max(0, duration-endMargin))
	}
	p.times[0] = start
	if p.frames > 1 {
		span := end - start
		if span <= 0 || float64(p.frames)/span > maxSheetRate {
			return fmt.Errorf("%w: %d frames from %v to %v", errInvalidSheet, p.frames, start, end)
		}
		step := span / float64(p.frames)
		width, p.rate = span+step, float64(p.frames)/span
		for i := range p.times {
			p.times[i] = start + float64(i)*step
		}
	}
	p.start, p.seek, p.window = start, max(0, start-lookback), width+lookback
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
	case probe.HasAlpha && !stickerSized(probe):
		// libvpx is not bounded by -max_pixels, so it never gets more than a sticker.
		bad = fmt.Sprintf("alpha on a %dx%d, %v s probe, more than a sticker", probe.Width, probe.Height, probe.Duration)
	case !usableSize(probe.Width, probe.Height):
		bad = fmt.Sprintf("size %dx%d", probe.Width, probe.Height)
	case !finiteIn(probe.Duration, 0, maxSheetSeconds):
		bad = fmt.Sprintf("duration %v", probe.Duration)
	case !finiteIn(probe.StartTime, -maxStartSeconds, maxStartSeconds) || !finiteIn(probe.ContainerStart, -maxStartSeconds, maxStartSeconds):
		bad = fmt.Sprintf("start times %v and %v", probe.StartTime, probe.ContainerStart)
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
// The encoder gets encoderThreads: by default it starts a thread per CPU (up
// to 16), and with glibc each thread reserves a malloc arena and a stack, so
// on a 32-CPU host they alone took ~0.5 GiB of the address space that
// toolMemoryLimit caps. The decoder a sticker's alpha forces is the only
// libvpx the whitelist ever holds (Ruling 16 item 5).
func renderArgs(input string, p sheetPlan, font string, timeLimit int) []string {
	codecs := renderCodecs
	if p.decoder != "" {
		codecs += "," + p.decoder
	}
	return toolArgs(toolRun{
		tool:      "ffmpeg",
		formats:   p.demuxer,
		demuxer:   p.demuxer,
		codecs:    codecs,
		decoder:   p.decoder,
		timeLimit: timeLimit,
		seek:      p.inputSeek,
		window:    p.window,
		input:     input,
		output: []string{"-map", "0:V:0", "-an", "-sn", "-dn", "-vf", sheetFilter(p, font),
			"-frames:v", "1", "-threads", strconv.Itoa(encoderThreads), "-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1"},
	})
}

// alphaOnGray flattens a transparent frame onto mid-gray. Pinning yuva420p
// before split keeps format negotiation from dropping the alpha, which it
// does once -ss or -t is set.
const alphaOnGray = "format=yuva420p,split[fg][bgsrc];[bgsrc]lutyuv=y=128:u=128:v=128:a=255[bg];[bg][fg]overlay=format=yuv420"

// nothingBeforeStart passes frames only once one at or before Start (time 0
// after the shift) has passed: otherwise the run produces no image.
const nothingBeforeStart = "select='lte(t,0)+gt(selected_n,0)'"

// timestampText is drawtext's only text: the frame's time (item 11).
const timestampText = `text='%{pts\:hms}'`

// sheetFilter builds the -vf graph from constants, the plan's checked numbers
// and the escaped font path; nothing in it comes from the file, the user or
// the model (item 11). With -copyts the frames keep the file's clock.
func sheetFilter(p sheetPlan, font string) string {
	var f []string
	switch {
	case p.rate > 0:
		// Shift Start to 0 (frames from the lookback get negative times), take
		// one frame every 1/rate s with round=up, which keeps the frame on
		// screen at each tile's time, then shift to Start in a fine time base
		// so drawtext prints the time from the video's start. One frame works
		// the same way.
		f = append(f, "setpts=PTS"+signedSeconds(-(p.origin+p.start))+"/TB")
		if p.seek > 0 {
			// Mid-file, the window must hold the frame on screen at Start. If it
			// does not (that frame began over the lookback earlier), drop
			// everything: input -t counts from the first frame after the seek
			// point, so later frames do arrive, and fps would copy the first of
			// them back onto the earlier tiles.
			f = append(f, nothingBeforeStart)
		}
		f = append(f, "fps=fps="+strconv.FormatFloat(p.rate, 'f', -1, 64)+":start_time=0:round=up",
			"settb=AVTB", "setpts=PTS"+signedSeconds(p.start)+"/TB")
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

// cpuTimeLimit is a run's CPU-time limit: twice its wall-clock timeout in
// whole seconds, a backstop under the context kill (item 10). On Linux the
// helper sets it as RLIMIT_CPU for both tools; elsewhere ffmpeg gets it as
// -timelimit.
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

// The gate's one version check (Ruling 12 item 6).
const (
	versionTimeout = 5 * time.Second
	minFFmpegMajor = 8   // older releases lack fixes the security decisions rely on
	maxVersionLine = 200 // bytes of the version line kept for the log
)

// ffmpegVersionLine reads the major version from the first line of
// `ffmpeg -version`, such as "ffmpeg version 8.1.3 Copyright …" or
// "ffmpeg version n7.1 …". Git builds ("N-118000-g…") have none.
var ffmpegVersionLine = regexp.MustCompile(`^ffmpeg version n?(\d{1,4})\.`)

// toolVersion runs `ffmpeg -hide_banner -version` once through runTool,
// within versionTimeout and in a fresh directory holding one empty file (the
// shape every run's input directory must have, Ruling 16 item 4), and
// returns the first line of its output, cut to maxVersionLine bytes.
func toolVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "ffmpeg-version-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "none"), nil, 0o600); err != nil {
		return "", err
	}
	out, err := runTool(ctx, "ffmpeg", dir, "", []string{"-hide_banner", "-version"})
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("ffmpeg -version printed nothing")
	}
	return line[:min(len(line), maxVersionLine)], nil
}

// ffmpegMajor is the major version in a toolVersion line, when it has one.
func ffmpegMajor(line string) (int, bool) {
	m := ffmpegVersionLine.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	major, err := strconv.Atoi(m[1])
	return major, err == nil
}

// toolSlots bounds concurrent ffprobe/ffmpeg processes across the whole bot
// (item 8). Each run takes a slot for itself alone: never across two runs.
var toolSlots = make(chan struct{}, toolConcurrency(runtime.NumCPU()))

// maxToolConcurrency is the most runs toolSlots ever allows at once; the
// task cap is sized for that many (toolThreadHeadroom, sandbox_linux.go).
const maxToolConcurrency = 4

// toolConcurrency is max(1, min(maxToolConcurrency, cpus/2)).
func toolConcurrency(cpus int) int {
	return max(1, min(maxToolConcurrency, cpus/2))
}

// toolError is a run that ended with a non-zero exit or a signal. Only the
// exit status decides failure (normal files print "Codec (aac) not on
// whitelist"). Its message is the tool and exit status alone; the stderr
// tail, which can hold text from the file, is only reachable through Stderr.
type toolError struct {
	tool   string
	err    error
	stderr string
}

func (e *toolError) Error() string { return e.tool + ": " + e.err.Error() }

func (e *toolError) Unwrap() error { return e.err }

// Stderr is the tail of the tool's stderr. It is for logs only: never send
// it to a user or a model.
func (e *toolError) Stderr() string { return e.stderr }

// toolStderr is the stderr tail of a failed run wrapped in err, or "". Log
// lines about a failed probe or render add it with %q.
func toolStderr(err error) string {
	var te *toolError
	if errors.As(err, &te) {
		return te.Stderr()
	}
	return ""
}

// runTool runs one ffprobe/ffmpeg process: an absolute binary, run in dir
// (the directory that holds only the input) inside the sandbox, which lets it
// read beneath dir and the font file font ("" for none) and nothing of the
// bot's. See runToolCmd for the rest.
func runTool(ctx context.Context, name, dir, font string, args []string) ([]byte, error) {
	bin, err := lookupTool(name)
	if err != nil {
		return nil, err
	}
	return runToolCmd(ctx, name, func(ctx context.Context) (*exec.Cmd, error) {
		return newToolCmd(ctx, bin, dir, font, args)
	})
}

// runToolCmd runs the process build makes: with a slot from toolSlots
// (waiting counts against ctx), stdout bounded at toolOutputLimit and the
// last toolStderrTail bytes of stderr kept. No retries.
func runToolCmd(ctx context.Context, name string, build func(context.Context) (*exec.Cmd, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := build(runCtx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	select {
	case toolSlots <- struct{}{}:
		defer func() { <-toolSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: waiting for a slot: %w", name, ctx.Err())
	}

	stdout := &boundedBuffer{limit: toolOutputLimit, onOverflow: cancel}
	stderr := &tailBuffer{max: toolStderrTail}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err = cmd.Start(); err == nil {
		endToolGroup(cmd) // on Linux: whatever the tool started dies with it (Ruling 16 item 3)
		err = cmd.Wait()
	}
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

// newToolCmd builds the child process for bin. On Linux it runs bin through
// the sandbox helper (ffmpeg_linux.go, sandbox_linux.go); elsewhere directly,
// once a bot allowed that. It fails with errNoSandbox when neither can be.
// On Linux the run's CPU time is capped at twice ctx's timeout, as
// RLIMIT_CPU (cpuTimeLimit; Ruling 16 item 2, Ruling 19 item 7).
func newToolCmd(ctx context.Context, bin, dir, font string, args []string) (*exec.Cmd, error) {
	path, argv, err := toolCommand(bin, dir, font, cpuTimeLimit(ctx), args)
	if err != nil {
		return nil, err
	}
	return toolProcess(ctx, dir, path, argv...), nil
}

// toolProcess is a child of the ffmpeg layer: its stdin stays nil (the null
// device), its environment holds only PATH (and SYSTEMROOT on Windows), and
// it runs in dir, the directory that holds only the input file (item 9). On
// Linux it leads its own process group, and a timeout kills that group
// (ffmpeg_linux.go).
func toolProcess(ctx context.Context, dir, path string, argv ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, argv...) // #nosec G204 -- fixed binary, generated arguments
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

// tailBuffer keeps the last max bytes written to it. A write appends, and
// only once the buffer holds twice max does it drop all but the last max,
// so each byte is copied a bounded number of times however small the writes
// (Ruling 21 item 3): trimming on every write moved the whole tail each
// time, and a tool writing stderr a byte at a time cost the bot a core.
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
	if len(t.buf) >= 2*t.max {
		t.buf = t.buf[:copy(t.buf, t.buf[len(t.buf)-t.max:])]
	}
	return n, nil
}

// String is the kept tail: the last max bytes written.
func (t *tailBuffer) String() string { return string(t.buf[max(0, len(t.buf)-t.max):]) }

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

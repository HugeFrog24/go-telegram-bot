package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireFFmpeg skips the test when ffmpeg or ffprobe is not on PATH.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// ffmpegFixture runs the system ffmpeg with args to write a test clip into its
// own new directory, under an extensionless name like a download. args must
// pick the muxer with -f, since the name has no extension.
func ffmpegFixture(t *testing.T, args ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "media")
	cmd := exec.Command("ffmpeg", append(append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...), out)...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture %v: %v\n%s", args, err, msg)
	}
	return out
}

// textFixture writes content to name in its own new directory.
func textFixture(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// Fixture recipes shared by the probe and render tests.
var (
	mp4Args  = []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=3", "-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", "-f", "mp4"}
	webmArgs = []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2", "-f", "lavfi", "-i", "sine=duration=2", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-c:a", "libopus", "-shortest", "-f", "webm"}
	gifArgs  = []string{"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=2", "-f", "gif"}
)

func TestExtForFormat(t *testing.T) {
	for format, want := range map[string]string{
		"mov,mp4,m4a,3gp,3g2,mj2": "mp4",
		"matroska,webm":           "webm",
		"gif":                     "gif",
		"gif_pipe":                "",
		"mov":                     "",
		"matroska":                "",
		"concat":                  "",
		"dash":                    "",
		"":                        "",
	} {
		assert.Equal(t, want, extForFormat(format), "extForFormat(%q)", format)
	}
}

func TestProbeMedia_AcceptsAllowedFormats(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	for _, tc := range []struct {
		name, kind, format, codec, ext string
		args                           []string
		duration                       float64
		w, h                           int
	}{
		{"mp4 h264+aac", "video", "mov,mp4,m4a,3gp,3g2,mj2", "h264", "mp4", mp4Args, 3, 320, 240},
		{"webm vp9+opus", "video", "matroska,webm", "vp9", "webm", webmArgs, 2, 320, 240},
		{"gif", "animation", "gif", "gif", "gif", gifArgs, 2, 160, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := probeMedia(context.Background(), ffmpegFixture(t, tc.args...), tc.kind)
			require.NoError(t, err)
			assert.Equal(t, tc.format, p.Format)
			assert.Equal(t, tc.codec, p.Codec)
			assert.Equal(t, tc.ext, extForFormat(p.Format))
			assert.InDelta(t, tc.duration, p.Duration, 0.15)
			assert.Equal(t, [2]int{tc.w, tc.h}, [2]int{p.Width, p.Height})
			assert.False(t, p.HasAlpha)
		})
	}
}

func TestProbeMedia_RejectsOthers(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	target := ffmpegFixture(t, gifArgs...)
	for name, path := range map[string]string{
		"text file":     textFixture(t, "notes.txt", "just some text\n"),
		"audio-only":    ffmpegFixture(t, "-f", "lavfi", "-i", "sine=duration=1", "-f", "wav"),
		"hls playlist":  textFixture(t, "list.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n"+target+"\n#EXT-X-ENDLIST\n"),
		"empty file":    textFixture(t, "media", ""),
		"missing file":  filepath.Join(t.TempDir(), "media"),
		"truncated mp4": truncatedFixture(t, ffmpegFixture(t, mp4Args...)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := probeMedia(context.Background(), path, "document")
			assert.ErrorIs(t, err, errUnsupportedMedia)
		})
	}
}

// truncatedFixture copies the first 2 KiB of src into a new directory.
func truncatedFixture(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	return textFixture(t, "media", string(data[:2048]))
}

// hasDemuxer reports whether the local ffmpeg build has the named demuxer.
func hasDemuxer(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-demuxers").Output()
	require.NoError(t, err)
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == name {
			return true
		}
	}
	return false
}

// The format whitelist (not file extensions, and not the demuxers' own
// checks) must refuse playlist and concat inputs that would make ffmpeg open
// other files. The fixtures are extensionless like downloads, and point at a
// harmless clip of our own.
func TestProbeMedia_RejectsPlaylistsAndConcat(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	target := ffmpegFixture(t, mp4Args...)
	for _, tc := range []struct{ demuxer, content string }{
		{"dash", `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT3S" minBufferTime="PT1S" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
<Period><AdaptationSet mimeType="video/mp4"><Representation id="1" bandwidth="1000"><BaseURL>` + target + `</BaseURL></Representation></AdaptationSet></Period>
</MPD>
`},
		{"concat", "ffconcat version 1.0\nfile '" + target + "'\n"},
	} {
		t.Run(tc.demuxer, func(t *testing.T) {
			path := textFixture(t, "media", tc.content)
			for _, kind := range []string{"video", "document"} {
				_, err := probeMedia(context.Background(), path, kind)
				require.ErrorIs(t, err, errUnsupportedMedia)
				var te *toolError
				assert.ErrorAs(t, err, &te, "rejected by a non-zero exit")
				if hasDemuxer(t, tc.demuxer) {
					assert.Contains(t, err.Error(), "Format not on whitelist")
				}
			}
		})
	}
}

func TestProbeMedia_KindPicksFormatWhitelist(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	t.Run("unknown kind runs nothing", func(t *testing.T) {
		_, err := probeMedia(context.Background(), filepath.Join(t.TempDir(), "media"), "audio")
		assert.ErrorIs(t, err, errUnsupportedMedia)
		assert.Contains(t, err.Error(), `unknown media kind "audio"`)
	})

	requireFFmpeg(t)
	mp4, webm, gif := ffmpegFixture(t, mp4Args...), ffmpegFixture(t, webmArgs...), ffmpegFixture(t, gifArgs...)
	for _, tc := range []struct {
		kind, path string
		ok         bool
	}{
		{"video", mp4, true}, {"video", webm, true}, {"video", gif, false},
		{"video_note", mp4, true}, {"video_note", webm, true}, {"video_note", gif, false},
		{"animation", mp4, true}, {"animation", gif, true}, {"animation", webm, false},
		{"document", mp4, true}, {"document", webm, true}, {"document", gif, true},
		{"sticker", webm, true}, {"sticker", mp4, false}, {"sticker", gif, false},
	} {
		_, err := probeMedia(context.Background(), tc.path, tc.kind)
		if tc.ok {
			assert.NoError(t, err, "%s %s", tc.kind, tc.path)
			continue
		}
		assert.ErrorIs(t, err, errUnsupportedMedia, "%s %s", tc.kind, tc.path)
		assert.Contains(t, err.Error(), "Format not on whitelist", "%s %s", tc.kind, tc.path)
	}
}

func TestProbeMedia_DecoderAllowlist(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	clip := ffmpegFixture(t, mp4Args...)
	cover := ffmpegFixture(t, "-f", "lavfi", "-i", "color=c=red:size=64x64", "-frames:v", "1", "-c:v", "png", "-f", "image2")
	srt := textFixture(t, "subs.srt", "1\n00:00:00,000 --> 00:00:01,000\nhello\n")
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"mpeg4 video", []string{"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=1", "-c:v", "mpeg4", "-f", "mp4"}, false},
		{"wmav2 audio", []string{"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=1", "-f", "lavfi", "-i", "sine=duration=1", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "wmav2", "-shortest", "-f", "matroska"}, false},
		{"png cover art", []string{"-i", clip, "-i", cover, "-map", "0", "-map", "1", "-c", "copy", "-disposition:v:1", "attached_pic", "-f", "mp4"}, false},
		{"pcm_s24be audio in mov", []string{"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=1", "-f", "lavfi", "-i", "sine=duration=1", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "pcm_s24be", "-shortest", "-f", "mov"}, true},
		{"srt subtitles in mkv", []string{"-i", clip, "-i", srt, "-map", "0", "-map", "1", "-c:v", "copy", "-c:a", "copy", "-c:s", "srt", "-f", "matroska"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := probeMedia(context.Background(), ffmpegFixture(t, tc.args...), "document")
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, errUnsupportedMedia)
			assert.Contains(t, err.Error(), "not on whitelist")
		})
	}
}

func TestProbeMedia_Caps(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	maps := []string{"-f", "lavfi", "-i", "testsrc2=size=32x32:rate=1:duration=1"}
	for range 33 {
		maps = append(maps, "-map", "0:v")
	}
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		// -max_pixels makes ffprobe report 0×0 for this frame; the Go check then refuses it.
		{"over -max_pixels (4100×4100)", "0x0 is not a usable size", []string{"-f", "lavfi", "-i", "testsrc2=size=4100x4100:rate=1", "-frames:v", "1", "-f", "gif"}},
		{"side over 8192 (8200×16)", "8200x16 is not a usable size", []string{"-f", "lavfi", "-i", "testsrc2=size=8200x16:rate=1", "-frames:v", "1", "-f", "gif"}},
		// -max_streams makes the demuxer stop at 32 streams; the Go check refuses a file at the cap.
		{"33 streams (-max_streams 32)", "32 streams", append(maps, "-c:v", "libx264", "-preset", "ultrafast", "-f", "matroska")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := probeMedia(context.Background(), ffmpegFixture(t, tc.args...), "document")
			require.ErrorIs(t, err, errUnsupportedMedia)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestProbeMedia_RotationAndAlpha(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	coded := ffmpegFixture(t, mp4Args...)
	rotated := ffmpegFixture(t, "-display_rotation", "90", "-i", coded, "-c", "copy", "-f", "mp4")
	p, err := probeMedia(context.Background(), rotated, "video")
	require.NoError(t, err)
	assert.Equal(t, [2]int{240, 320}, [2]int{p.Width, p.Height}, "display size, as autorotate decodes it")

	alpha := ffmpegFixture(t, alphaWebmArgs(512, 3)...)
	p, err = probeMedia(context.Background(), alpha, "sticker")
	require.NoError(t, err)
	assert.True(t, p.HasAlpha)
	assert.Equal(t, "vp9", p.Codec)
	assert.Equal(t, [2]int{512, 512}, [2]int{p.Width, p.Height})
}

// alphaWebmArgs makes a fully transparent red VP9 clip: decoded with its
// alpha it must come out as the gray background, never red.
func alphaWebmArgs(size, seconds int) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=red:size=%dx%d:rate=30:duration=%d,format=rgba,colorchannelmixer=aa=0", size, size, seconds),
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "-deadline", "realtime", "-cpu-used", "8", "-f", "webm"}
}

func TestParseProbe(t *testing.T) {
	const mov = "mov,mp4,m4a,3gp,3g2,mj2"
	video := func(codec string, w, h int, duration, extra string) string {
		return fmt.Sprintf(`{"codec_type":"video","codec_name":%q,"width":%d,"height":%d,"duration":%q,"disposition":{"attached_pic":0}%s}`,
			codec, w, h, duration, extra)
	}
	h264 := video("h264", 720, 1280, "37.000000", "")
	doc := func(format, duration string, streams ...string) []byte {
		return []byte(`{"streams":[` + strings.Join(streams, ",") + `],"format":{"format_name":"` + format + `","duration":"` + duration + `"}}`)
	}
	cover := `{"codec_type":"video","codec_name":"mjpeg","width":3000,"height":3000,"disposition":{"attached_pic":1}}`
	audio := `{"codec_type":"audio","codec_name":"aac","disposition":{"attached_pic":0}}`
	portrait := mediaProbe{Format: mov, Codec: "h264", Duration: 37, Width: 720, Height: 1280}
	with := func(p mediaProbe, edit func(*mediaProbe)) mediaProbe { edit(&p); return p }

	t.Run("accepted", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			out  []byte
			want mediaProbe
		}{
			{"plain", doc(mov, "37.1", h264), portrait},
			{"cover art first is skipped", doc(mov, "37.1", cover, audio, h264), portrait},
			{"stream duration unknown: format duration", doc(mov, "36.5", video("h264", 720, 1280, "N/A", "")),
				with(portrait, func(p *mediaProbe) { p.Duration = 36.5 })},
			{"no duration anywhere: 0", doc(mov, "N/A", video("h264", 720, 1280, "N/A", "")),
				with(portrait, func(p *mediaProbe) { p.Duration = 0 })},
			{"rotated -90: display size", doc(mov, "37", video("h264", 720, 1280, "37", `,"side_data_list":[{"rotation":-90}]`)),
				with(portrait, func(p *mediaProbe) { p.Width, p.Height = 1280, 720 })},
			{"rotated 180: same size", doc(mov, "37", video("h264", 720, 1280, "37", `,"side_data_list":[{"rotation":180}]`)), portrait},
			{"vp9 alpha", doc("matroska,webm", "3", video("vp9", 512, 512, "N/A", `,"tags":{"alpha_mode":"1"}`)),
				mediaProbe{Format: "matroska,webm", Codec: "vp9", Duration: 3, Width: 512, Height: 512, HasAlpha: true}},
			{"alpha tag on h264 is ignored", doc("matroska,webm", "3", video("h264", 512, 512, "N/A", `,"tags":{"alpha_mode":"1"}`)),
				mediaProbe{Format: "matroska,webm", Codec: "h264", Duration: 3, Width: 512, Height: 512}},
			{"8192 side and 16777216 pixels", doc(mov, "1", video("hevc", 8192, 2048, "1", "")),
				mediaProbe{Format: mov, Codec: "hevc", Duration: 1, Width: 8192, Height: 2048}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := parseProbe(tc.out, []string{"mov", "matroska"})
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			})
		}
	})

	t.Run("refused", func(t *testing.T) {
		many := make([]string, toolMaxStreams)
		for i := range many {
			many[i] = h264
		}
		for _, tc := range []struct {
			name, want string
			out        []byte
		}{
			{"not JSON", "parse", []byte("Format not on whitelist")},
			{"format outside the kind's list", `format "gif"`, doc("gif", "1", video("gif", 64, 64, "1", ""))},
			{"format only prefix-like", `format "mov,mp4,m4a,3gp,3g2,mj2x"`, doc(mov+"x", "1", h264)},
			{"no video stream", "no video stream", doc(mov, "1", audio)},
			{"only cover art", "no video stream", doc(mov, "1", cover, audio)},
			{"first video stream's codec not allowed", `codec "mpeg4"`, doc(mov, "1", video("mpeg4", 720, 1280, "1", ""), h264)},
			{"mjpeg video stream", `codec "mjpeg"`, doc(mov, "1", video("mjpeg", 720, 1280, "1", ""))},
			{"area over the cap", "5000x4000 is not a usable size", doc(mov, "1", video("h264", 5000, 4000, "1", ""))},
			{"side over the cap", "8193x16 is not a usable size", doc(mov, "1", video("h264", 8193, 16, "1", ""))},
			{"zero size", "0x0 is not a usable size", doc(mov, "1", video("h264", 0, 0, "1", ""))},
			{"stream count at the cap", "32 streams", doc(mov, "1", many...)},
			{"NaN duration", "duration", doc(mov, "1", video("h264", 720, 1280, "NaN", ""))},
			{"infinite duration", "duration", doc(mov, "+Inf", video("h264", 720, 1280, "N/A", ""))},
			{"negative duration", "duration", doc(mov, "1", video("h264", 720, 1280, "-3", ""))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := parseProbe(tc.out, []string{"mov", "matroska"})
				assert.ErrorIs(t, err, errUnsupportedMedia)
				assert.Contains(t, err.Error(), tc.want)
			})
		}
	})
}

func TestToolArgs_Probe(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	in := filepath.Join(t.TempDir(), "media")
	got := toolArgs(toolRun{tool: "ffprobe", formats: "mov,matroska", codecs: probeCodecs, input: in, output: probeOutput})
	assert.Equal(t, []string{
		"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska",
		"-codec_whitelist", probeCodecs, "-max_pixels", "16777216", "-max_streams", "32", "-threads", "2",
		"-i", "file:" + in,
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,duration:stream_disposition=attached_pic:stream_tags=alpha_mode:stream_side_data=rotation",
		"-of", "json",
	}, got)
	assert.NotContains(t, got, "-nostdin", "ffprobe rejects -nostdin; its stdin stays nil")
	assert.NotContains(t, got, "-timelimit", "ffprobe rejects -timelimit")
	assertNoForbiddenArgs(t, got)
}

// assertNoForbiddenArgs checks the security decisions' never-list.
func assertNoForbiddenArgs(t *testing.T, args []string) {
	t.Helper()
	for _, a := range args {
		for _, bad := range []string{"-hwaccel", "-enable_drefs", "-use_absolute_path", "-safe", "-stream_loop", "-ignore_loop", "textfile", "movie=", "amovie="} {
			assert.NotContains(t, a, bad)
		}
	}
}

func TestDecoderLists(t *testing.T) {
	assert.Equal(t, "h264,hevc,vp8,vp9,libvpx,libvpx-vp9,libdav1d,libaom-av1,gif,"+
		"aac,aac_fixed,mp3,mp3float,mp2,mp2float,opus,libopus,vorbis,libvorbis,flac,alac,ac3,eac3,"+
		"amrnb,amrwb,libopencore_amrnb,libopencore_amrwb,adpcm_ima_qt,"+
		"pcm_s8,pcm_u8,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s24be,pcm_s32le,pcm_s32be,"+
		"pcm_f32le,pcm_f32be,pcm_f64le,pcm_f64be,pcm_mulaw,pcm_alaw,"+
		"mov_text,srt,subrip,ssa,ass,webvtt,text", probeCodecs)
	assert.Equal(t, "h264,hevc,vp8,vp9,libvpx,libvpx-vp9,libdav1d,libaom-av1,gif", renderCodecs)
	for _, list := range []string{probeCodecs, renderCodecs} {
		for _, name := range strings.Split(list, ",") {
			assert.NotContains(t, []string{"mjpeg", "png", "av1"}, name, "cover-art decoders and native av1 stay off")
		}
	}
}

func TestToolConcurrency(t *testing.T) {
	for cpus, want := range map[int]int{1: 1, 2: 1, 3: 1, 4: 2, 6: 3, 8: 4, 64: 4} {
		assert.Equal(t, want, toolConcurrency(cpus), "%d CPUs", cpus)
	}
	assert.Equal(t, toolConcurrency(runtime.NumCPU()), cap(toolSlots))
}

func TestNewToolCmd(t *testing.T) {
	dir := t.TempDir()
	cmd := newToolCmd(context.Background(), "/abs/ffmpeg", dir, []string{"-version"})
	assert.Equal(t, dir, cmd.Dir)
	assert.Equal(t, 2*time.Second, cmd.WaitDelay)
	assert.Nil(t, cmd.Stdin)
	want := []string{"PATH=" + os.Getenv("PATH")}
	if runtime.GOOS == "windows" {
		want = append(want, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	assert.Equal(t, want, cmd.Env, "only PATH (and SYSTEMROOT on Windows) reaches the child")
	if runtime.GOOS == "linux" {
		require.NotNil(t, cmd.SysProcAttr)
		assert.EqualValues(t, 9, reflect.ValueOf(cmd.SysProcAttr).Elem().FieldByName("Pdeathsig").Int(), "SIGKILL when the bot dies")
	}
}

func TestLookupTool(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := lookupTool("ffmpeg")
		assert.Error(t, err)
	})

	t.Run("relative", func(t *testing.T) {
		dir := t.TempDir()
		name := "faketool"
		file := name
		if runtime.GOOS == "windows" {
			file += ".exe"
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("not a program"), 0o700)) // #nosec G306 -- executable bit for LookPath
		t.Chdir(dir)
		t.Setenv("PATH", ".")
		_, err := lookupTool(name)
		assert.Error(t, err, "ErrDot")

		t.Setenv("GODEBUG", "execerrdot=0")
		if p, err := exec.LookPath(name); err == nil {
			assert.False(t, filepath.IsAbs(p), "LookPath gave a relative path without an error")
		}
		_, err = lookupTool(name)
		assert.Error(t, err, "relative result without ErrDot")
	})

	t.Run("found: absolute", func(t *testing.T) {
		requireFFmpeg(t)
		p, err := lookupTool("ffmpeg")
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(p), p)
	})
}

func TestTailBuffer(t *testing.T) {
	var tb tailBuffer
	tb.max = 8
	for _, s := range []string{"abc", "defgh", "ij", "klmnopqrstuvwxyz", "1"} {
		n, err := tb.Write([]byte(s))
		assert.NoError(t, err)
		assert.Equal(t, len(s), n)
	}
	assert.Equal(t, "tuvwxyz1", tb.String())
	assert.Equal(t, 64<<10, toolStderrTail)
}

func TestBoundedBuffer(t *testing.T) {
	var cancelled bool
	b := boundedBuffer{limit: 8, onOverflow: func() { cancelled = true }}
	_, err := b.Write([]byte("12345678"))
	require.NoError(t, err, "exactly the limit is fine")
	assert.False(t, cancelled)
	_, err = b.Write([]byte("9"))
	assert.ErrorIs(t, err, errToolOutputTooLarge, "one byte more is an error")
	assert.True(t, cancelled, "and kills the process")
	assert.True(t, b.overflow)
	assert.Equal(t, 8<<20, toolOutputLimit)
}

func TestRunTool_StdoutBound(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	raw := func(frames string) []string {
		return []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=4096x2048:rate=1",
			"-frames:v", frames, "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1"}
	}
	out, err := runTool(context.Background(), "ffmpeg", t.TempDir(), raw("1"))
	require.NoError(t, err, "exactly 8 MiB passes")
	assert.Len(t, out, toolOutputLimit)

	start := time.Now()
	out, err = runTool(context.Background(), "ffmpeg", t.TempDir(), raw("5"))
	assert.ErrorIs(t, err, errToolOutputTooLarge)
	assert.Nil(t, out)
	assert.Less(t, time.Since(start), 10*time.Second, "the process is killed, not drained")
}

func TestRunTool_FailureIsExitStatus(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	clip := ffmpegFixture(t, mp4Args...)

	// A normal clip with AAC audio prints "Codec (aac) not on whitelist" yet succeeds.
	var stderrSeen bytes.Buffer
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error", "-codec_whitelist", renderCodecs, "-i", "file:"+clip, "-map", "0:V:0", "-frames:v", "1", "-f", "null", "-")
	cmd.Stderr = &stderrSeen
	require.NoError(t, cmd.Run())
	require.Contains(t, stderrSeen.String(), "not on whitelist", "the noise this test is about")
	_, err := runTool(context.Background(), "ffmpeg", filepath.Dir(clip), cmd.Args[1:])
	assert.NoError(t, err, "stderr content alone is never a failure")

	// A failing run with nothing on stderr is still a failure.
	_, err = runTool(context.Background(), "ffmpeg", t.TempDir(), []string{"-hide_banner", "-nostdin", "-loglevel", "quiet", "-i", "file:" + filepath.Join(t.TempDir(), "missing"), "-f", "null", "-"})
	var te *toolError
	require.ErrorAs(t, err, &te)
	assert.Empty(t, te.stderr)
}

func TestRunTool_Semaphore(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	for range cap(toolSlots) {
		toolSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(toolSlots) > 0 {
			<-toolSlots
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runTool(ctx, "ffmpeg", t.TempDir(), []string{"-hide_banner", "-version"})
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the extra run waits for a slot, within its own timeout")
	assert.Less(t, time.Since(start), 3*time.Second)

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = runTool(cancelled, "ffmpeg", t.TempDir(), []string{"-hide_banner", "-version"})
	assert.ErrorIs(t, err, context.Canceled)

	<-toolSlots // one slot free: a run takes it and gives it back
	_, err = runTool(context.Background(), "ffmpeg", t.TempDir(), []string{"-hide_banner", "-version"})
	assert.NoError(t, err)
	assert.Equal(t, cap(toolSlots)-1, len(toolSlots))
}

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

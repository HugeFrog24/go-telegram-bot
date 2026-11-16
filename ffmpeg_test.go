package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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
	for line := range strings.SplitSeq(string(out), "\n") {
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
		for name := range strings.SplitSeq(list, ",") {
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

// decodeSheet decodes a rendered JPEG and checks it against the sizing rule.
func decodeSheet(t *testing.T, data []byte, maxEdge int) image.Image {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err, "a decodable JPEG")
	assertWithinImageLimits(t, cfg.Width, cfg.Height, maxEdge)
	img, err := jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	return img
}

// portraitClip is a 37 s 720×1280 H.264 clip with AAC audio.
func portraitClip(t *testing.T) (string, mediaProbe) {
	t.Helper()
	path := ffmpegFixture(t, "-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=25:duration=37", "-f", "lavfi", "-i", "sine=duration=37",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", "-f", "mp4")
	p, err := probeMedia(context.Background(), path, "video")
	require.NoError(t, err)
	require.InDelta(t, 37, p.Duration, 0.1)
	return path, p
}

func TestRenderSheet_SizesAndTimes(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path, probe := portraitClip(t)
	start := time.Now()
	data, times, err := renderSheet(context.Background(), path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 12, MaxEdge: maxImageEdge})
	elapsed := time.Since(start)
	require.NoError(t, err)
	t.Logf("12-frame sheet of a 37 s 720x1280 clip: %v, %d bytes", elapsed, len(data))
	assert.Less(t, elapsed, sheetTimeout/2, "well within the sheet timeout")

	img := decodeSheet(t, data, maxImageEdge)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	assert.Zero(t, w%4, "4 columns of equal tiles")
	assert.Zero(t, h%3, "3 rows of equal tiles")
	assert.InDelta(t, 720.0/1280.0, float64(w/4)/float64(h/3), 0.02, "tiles keep the aspect")

	require.Len(t, times, 12)
	for i, got := range times {
		assert.InDelta(t, float64(i)*probe.Duration/12, got, 1e-9, "t_%d", i)
	}
}

func TestRenderSheet_SingleFrame(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path, probe := portraitClip(t)
	data, times, err := renderSheet(context.Background(), path, probe, sheetSpec{Start: 12.5, End: 1, Frames: 1, MaxEdge: maxImageEdge})
	require.NoError(t, err, "a 1 s decode window after the seek point holds the frame")
	img := decodeSheet(t, data, maxImageEdge)
	assert.Equal(t, [2]int{720, 1280}, [2]int{img.Bounds().Dx(), img.Bounds().Dy()}, "already within the limits: not scaled")
	assert.Equal(t, []float64{12.5}, times)
}

func TestRenderSheet_SingleFrameGIF(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path := ffmpegFixture(t, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10", "-frames:v", "1", "-f", "gif")
	probe, err := probeMedia(context.Background(), path, "animation")
	require.NoError(t, err)

	data, times, err := renderSheet(context.Background(), path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 12, MaxEdge: maxImageEdge})
	require.NoError(t, err, "probed duration %v", probe.Duration)
	decodeSheet(t, data, maxImageEdge)
	assert.Len(t, times, 12)

	probe.Duration = 0
	data, times, err = renderSheet(context.Background(), path, probe, sheetSpec{Start: 0, End: 0, Frames: 12, MaxEdge: maxImageEdge})
	require.NoError(t, err, "unknown duration: no divide by zero")
	img := decodeSheet(t, data, maxImageEdge)
	assert.Equal(t, 160*4, img.Bounds().Dx(), "a full 4×3 grid")
	assert.Equal(t, make([]float64, 12), times, "every frame at t=0")
}

// tileCenter is the color a little below the middle of tile (col, row) of a
// cols×rows sheet, clear of the timestamp box.
func tileCenter(img image.Image, cols, rows, col, row int) (r, g, b uint32) {
	tw, th := img.Bounds().Dx()/cols, img.Bounds().Dy()/rows
	r, g, b, _ = img.At(col*tw+tw/2, row*th+th/2+th/4).RGBA()
	return r >> 8, g >> 8, b >> 8
}

func TestRenderSheet_PartialGridIsBlack(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path := ffmpegFixture(t, mp4Args...)
	probe, err := probeMedia(context.Background(), path, "video")
	require.NoError(t, err)
	data, times, err := renderSheet(context.Background(), path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 5, MaxEdge: maxImageEdge})
	require.NoError(t, err)
	assert.Len(t, times, 5)
	img := decodeSheet(t, data, maxImageEdge)
	r, g, b := tileCenter(img, 3, 2, 2, 1)
	assert.Less(t, r+g+b, uint32(30), "the sixth tile of a 3×2 grid is black padding, got %d,%d,%d", r, g, b)
	r, g, b = tileCenter(img, 3, 2, 1, 1)
	assert.Greater(t, r+g+b, uint32(60), "the fifth tile is a frame")
}

func TestRenderSheet_StickerAlpha(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path := ffmpegFixture(t, alphaWebmArgs(512, 3)...)
	probe, err := probeMedia(context.Background(), path, "sticker")
	require.NoError(t, err)
	require.True(t, probe.HasAlpha)

	spec := sheetSpec{Start: 0, End: probe.Duration, Frames: stickerSheetFrames, MaxEdge: stickerSheetEdge}
	data, times, err := renderSheet(context.Background(), path, probe, spec)
	require.NoError(t, err)
	img := decodeSheet(t, data, stickerSheetEdge)
	assert.Len(t, times, stickerSheetFrames)
	r, g, b := tileCenter(img, 3, 2, 0, 0)
	assert.InDelta(t, 128, r, 20, "transparent pixels show the gray background (got %d,%d,%d)", r, g, b)
	assert.InDelta(t, 128, g, 20)
	assert.InDelta(t, 128, b, 20)

	// Without the alpha decoder the same clip comes out red: the gray is the alpha path's doing.
	probe.HasAlpha = false
	data, _, err = renderSheet(context.Background(), path, probe, spec)
	require.NoError(t, err)
	r, g, _ = tileCenter(decodeSheet(t, data, stickerSheetEdge), 3, 2, 0, 0)
	assert.Greater(t, int(r)-int(g), 100, "red without alpha")
}

func TestRenderSheet_Timeout(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path := ffmpegFixture(t, gifArgs...)
	probe, err := probeMedia(context.Background(), path, "animation")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err = renderSheet(ctx, path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: maxImageEdge})
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, errUnsupportedMedia, "a timeout is not a decode failure")
	assert.Less(t, time.Since(start), time.Second)
}

func TestRenderSheet_RefusesBadInput(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	good := mediaProbe{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", Duration: 37, Width: 720, Height: 1280}
	spec := sheetSpec{Start: 0, End: 37, Frames: 12, MaxEdge: maxImageEdge}
	nan, inf := math.NaN(), math.Inf(1)
	for _, tc := range []struct {
		name  string
		probe func(*mediaProbe)
		spec  func(*sheetSpec)
	}{
		{"NaN start", nil, func(s *sheetSpec) { s.Start = nan }},
		{"+Inf end", nil, func(s *sheetSpec) { s.End = inf }},
		{"-Inf start", nil, func(s *sheetSpec) { s.Start = -inf }},
		{"negative start", nil, func(s *sheetSpec) { s.Start = -1 }},
		{"NaN end on a single frame", nil, func(s *sheetSpec) { s.Frames, s.End = 1, nan }},
		{"end before start", nil, func(s *sheetSpec) { s.Start, s.End = 20, 10 }},
		{"empty range", nil, func(s *sheetSpec) { s.Start, s.End = 10, 10 }},
		{"range too short for the frames", nil, func(s *sheetSpec) { s.Start, s.End = 10, 10.001 }},
		{"start out of range", nil, func(s *sheetSpec) { s.Start = 1e12 }},
		{"no frames", nil, func(s *sheetSpec) { s.Frames = 0 }},
		{"too many frames", nil, func(s *sheetSpec) { s.Frames = maxSheetFrames + 1 }},
		{"edge too small", nil, func(s *sheetSpec) { s.MaxEdge = 0 }},
		{"edge over the limit", nil, func(s *sheetSpec) { s.MaxEdge = maxImageEdge + 1 }},
		{"NaN duration", func(p *mediaProbe) { p.Duration = nan }, nil},
		{"infinite duration", func(p *mediaProbe) { p.Duration = inf }, nil},
		{"negative duration", func(p *mediaProbe) { p.Duration = -1 }, nil},
		{"duration out of range", func(p *mediaProbe) { p.Duration = maxSheetSeconds + 1 }, nil},
		{"concat format", func(p *mediaProbe) { p.Format = "concat" }, nil},
		{"lavfi format", func(p *mediaProbe) { p.Format = "lavfi" }, nil},
		{"mpeg4 codec", func(p *mediaProbe) { p.Codec = "mpeg4" }, nil},
		{"zero width", func(p *mediaProbe) { p.Width = 0 }, nil},
		{"width over the cap", func(p *mediaProbe) { p.Width = probeMaxSide + 1 }, nil},
		{"alpha on h264", func(p *mediaProbe) { p.HasAlpha = true }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, s := good, spec
			if tc.probe != nil {
				tc.probe(&p)
			}
			if tc.spec != nil {
				tc.spec(&s)
			}
			_, _, err := renderSheet(context.Background(), filepath.Join(t.TempDir(), "media"), p, s)
			assert.ErrorIs(t, err, errInvalidSheet)
		})
	}
}

func TestPlanSheet(t *testing.T) {
	probe := mediaProbe{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", Duration: 37, Width: 720, Height: 1280}

	t.Run("sheet: decode window is the range plus one step", func(t *testing.T) {
		p, err := planSheet(probe, sheetSpec{Start: 6, End: 18, Frames: 12, MaxEdge: maxImageEdge})
		require.NoError(t, err)
		assert.Equal(t, 6.0, p.seek)
		assert.InDelta(t, 13, p.window, 1e-9, "12 s range + 1 s step")
		assert.InDelta(t, 1, p.rate, 1e-9)
		assert.Equal(t, [2]int{4, 3}, [2]int{p.cols, p.rows})
		assert.Zero(t, p.tileW%2)
		assert.Zero(t, p.tileH%2)
		assertWithinImageLimits(t, p.cols*p.tileW, p.rows*p.tileH, maxImageEdge)
		assert.Equal(t, "mov", p.demuxer)
		assert.Empty(t, p.decoder)
	})

	t.Run("single frame: a fixed small window", func(t *testing.T) {
		p, err := planSheet(probe, sheetSpec{Start: 12.5, End: 0, Frames: 1, MaxEdge: maxImageEdge})
		require.NoError(t, err)
		assert.Equal(t, 12.5, p.seek)
		assert.Equal(t, singleFrameWindow, p.window)
		assert.Zero(t, p.rate)
		assert.Equal(t, []float64{12.5}, p.times)
	})

	t.Run("range clamped to the duration", func(t *testing.T) {
		p, err := planSheet(probe, sheetSpec{Start: 30, End: 300, Frames: 7, MaxEdge: maxImageEdge})
		require.NoError(t, err)
		assert.InDelta(t, 8, p.window, 1e-9, "7 s range + 1 s step")
		assert.InDelta(t, 36, p.times[6], 1e-9)
	})

	t.Run("unknown duration: every frame at 0", func(t *testing.T) {
		unknown := probe
		unknown.Duration = 0
		p, err := planSheet(unknown, sheetSpec{Start: 5, End: 9, Frames: 6, MaxEdge: maxImageEdge})
		require.NoError(t, err)
		assert.Equal(t, make([]float64, 6), p.times)
		assert.Equal(t, 0.0, p.seek)
		assert.Equal(t, singleFrameWindow, p.window)
		assert.Zero(t, p.rate)
	})

	t.Run("alpha sticker", func(t *testing.T) {
		sticker := mediaProbe{Format: "matroska,webm", Codec: "vp9", Duration: 3, Width: 512, Height: 512, HasAlpha: true}
		p, err := planSheet(sticker, sheetSpec{Start: 0, End: 3, Frames: stickerSheetFrames, MaxEdge: stickerSheetEdge})
		require.NoError(t, err)
		assert.Equal(t, "libvpx-vp9", p.decoder)
		assert.Equal(t, "matroska", p.demuxer)
		assertWithinImageLimits(t, p.cols*p.tileW, p.rows*p.tileH, stickerSheetEdge)
	})
}

func TestToolArgs_Render(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	in := filepath.Join(t.TempDir(), "media")
	probe := mediaProbe{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", Duration: 37, Width: 720, Height: 1280}
	plan, err := planSheet(probe, sheetSpec{Start: 6, End: 18, Frames: 12, MaxEdge: maxImageEdge})
	require.NoError(t, err)
	got := renderArgs(in, plan, `C:\Windows\Fonts\arial.ttf`, 120)
	assert.Equal(t, []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-timelimit", "120", "-filter_threads", "1", "-copyts",
		"-protocol_whitelist", "file", "-format_whitelist", "mov", "-f", "mov",
		"-codec_whitelist", renderCodecs, "-max_pixels", "16777216", "-max_streams", "32", "-threads", "2",
		"-ss", "6.000000", "-t", "13.000000",
		"-i", "file:" + in,
		"-map", "0:V:0", "-an", "-sn", "-dn",
		"-vf", "setpts=PTS-6.000000/TB,fps=fps=1:start_time=0:round=up,settb=AVTB,setpts=PTS+6.000000/TB,trim=end_frame=12," +
			"scale=238:422," +
			`drawtext=fontfile=C\\:\\\\Windows\\\\Fonts\\\\arial.ttf:text='%{pts\:hms}':fontsize=19:fontcolor=white:box=1:boxcolor=black@0.6:boxborderw=4:x=4:y=4,` +
			"tile=4x3",
		"-frames:v", "1", "-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1",
	}, got)
	assertNoForbiddenArgs(t, got)

	t.Run("alpha, unknown duration, no font", func(t *testing.T) {
		sticker := mediaProbe{Format: "matroska,webm", Codec: "vp9", Width: 512, Height: 512, HasAlpha: true}
		plan, err := planSheet(sticker, sheetSpec{Frames: 6, MaxEdge: stickerSheetEdge})
		require.NoError(t, err)
		got := renderArgs(in, plan, "", 40)
		i := slices.Index(got, "-i")
		assert.Equal(t, []string{"-c:v", "libvpx-vp9", "-ss", "0.000000", "-t", "1.000000"}, got[i-6:i], "decoder and window are input options")
		assert.Equal(t, "trim=end_frame=1,loop=loop=5:size=1,setpts=0,trim=end_frame=6,"+
			"format=yuva420p,split[fg][bgsrc];[bgsrc]lutyuv=y=128:u=128:v=128:a=255[bg];[bg][fg]overlay=format=yuv420,"+
			"scale=170:170,"+
			`drawtext=text='%{pts\:hms}':fontsize=14:fontcolor=white:box=1:boxcolor=black@0.6:boxborderw=3:x=3:y=3,`+
			"tile=3x2", got[slices.Index(got, "-vf")+1])
		assertNoForbiddenArgs(t, got)
	})
}

func TestCPUTimeLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	assert.Equal(t, 40, cpuTimeLimit(ctx), "twice a lookup's 20 s")
	ctx2, cancel2 := context.WithTimeout(context.Background(), sheetTimeout)
	defer cancel2()
	assert.Equal(t, 120, cpuTimeLimit(ctx2), "twice a sheet's 60 s")
}

func TestRenderSheet_RenderAllowlists(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	forged := mediaProbe{Format: "mov,mp4,m4a,3gp,3g2,mj2", Codec: "h264", Duration: 1, Width: 160, Height: 120}
	gifProbe := mediaProbe{Format: "gif", Codec: "gif", Duration: 1, Width: 160, Height: 120}
	spec := sheetSpec{Start: 0, End: 1, Frames: 4, MaxEdge: maxImageEdge}
	for _, tc := range []struct {
		name  string
		path  string
		probe mediaProbe
	}{
		{"mpeg4 decoder is not on the render list", ffmpegFixture(t, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=1", "-c:v", "mpeg4", "-f", "mp4"), forged},
		{"-max_pixels stops a frame the probe never saw", ffmpegFixture(t, "-f", "lavfi", "-i", "testsrc2=size=4100x4100:rate=1", "-frames:v", "1", "-f", "gif"), gifProbe},
		{"-f is pinned from the probe", ffmpegFixture(t, gifArgs...), forged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := renderSheet(context.Background(), tc.path, tc.probe, spec)
			assert.ErrorIs(t, err, errUnsupportedMedia)
			var te *toolError
			assert.ErrorAs(t, err, &te, "ffmpeg exited non-zero")
		})
	}
}

func TestRenderSheet_NoNestedSlots(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	path := ffmpegFixture(t, gifArgs...)
	for range cap(toolSlots) - 1 {
		toolSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(toolSlots) > 0 {
			<-toolSlots
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probe, err := probeMedia(ctx, path, "animation")
	require.NoError(t, err, "one free slot is enough")
	_, _, err = renderSheet(ctx, path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: maxImageEdge})
	require.NoError(t, err, "one free slot is enough: a run never holds two")
	assert.Equal(t, cap(toolSlots)-1, len(toolSlots), "the slot came back")
}

func TestFilterValue(t *testing.T) {
	assert.Equal(t, `C\\:\\\\Windows\\\\Fonts\\\\arial.ttf`, filterValue(`C:\Windows\Fonts\arial.ttf`))
	assert.Equal(t, `/usr/share/fonts/it\\\'s\,\[a\]\;b.ttf`, filterValue(`/usr/share/fonts/it's,[a];b.ttf`))
	assert.Equal(t, "/usr/share/fonts/dejavu/DejaVuSans.ttf", filterValue("/usr/share/fonts/dejavu/DejaVuSans.ttf"))
	assert.Equal(t, "caf\xe9\xff", filterValue("caf\xe9\xff"), "bytes kept as they are")
}

// copyFile copies src to dst for a test.
func copyFile(t *testing.T, src, dst string) string {
	t.Helper()
	data, err := os.ReadFile(src) // #nosec G304 -- a system font chosen by the test
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, data, 0o600))
	return dst
}

// otherFont finds a second, differently drawn font next to the built-in one.
func otherFont(builtIn string) string {
	for _, name := range []string{"DejaVuSerif.ttf", "DejaVuSansMono.ttf", "times.ttf", "cour.ttf"} {
		if p := filepath.Join(filepath.Dir(builtIn), name); usableFontFile(p) {
			return p
		}
	}
	return ""
}

func TestRenderSheet_FontPathEscaping(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	requireFFmpeg(t)
	builtIn := resolveFontFile("")
	other := otherFont(builtIn)
	if builtIn == "" || other == "" {
		t.Skip("needs two built-in fonts")
	}
	dir := filepath.Join(t.TempDir(), "it's [a],b;c")
	require.NoError(t, os.Mkdir(dir, 0o700))
	odd := copyFile(t, other, filepath.Join(dir, "font's,[1];x.ttf"))
	require.Equal(t, odd, resolveFontFile(odd))
	t.Logf("font path: %s", odd) // on Windows it also holds ':' and '\'

	path := ffmpegFixture(t, gifArgs...)
	probe, err := probeMedia(context.Background(), path, "animation")
	require.NoError(t, err)
	render := func(font string) []byte {
		t.Helper()
		data, _, err := renderSheet(context.Background(), path, probe, sheetSpec{Start: 0, End: probe.Duration, Frames: 4, MaxEdge: maxImageEdge, FontFile: font})
		require.NoError(t, err, font)
		decodeSheet(t, data, maxImageEdge)
		return data
	}

	// drawtext quietly falls back to a default font when it cannot load
	// fontfile (through fontconfig on Linux), so a render that works proves
	// little. The odd path must draw exactly like the same font at a plain
	// path, and unlike the built-in font a fallback would use.
	plain := render(copyFile(t, other, filepath.Join(t.TempDir(), "plain.ttf")))
	assert.Equal(t, plain, render(odd), "drawtext loaded the font through both escaping levels")
	assert.NotEqual(t, plain, render(builtIn), "the two fonts draw differently")
}

func TestResolveFontFile(t *testing.T) {
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })

	builtIn := resolveFontFile("")
	good := filepath.Join(t.TempDir(), "font.ttf")
	require.NoError(t, os.WriteFile(good, []byte("not really a font"), 0o600))
	assert.Equal(t, good, resolveFontFile(good), "an absolute regular file is used as configured")
	assert.Empty(t, logged.String())

	for name, bad := range map[string]string{
		"relative":  "fonts/font.ttf",
		"directory": t.TempDir(),
		"missing":   filepath.Join(t.TempDir(), "missing.ttf"),
	} {
		logged.Reset()
		assert.Equal(t, builtIn, resolveFontFile(bad), name)
		assert.Equal(t, builtIn, resolveFontFile(bad), name)
		assert.Equal(t, 1, strings.Count(logged.String(), "video: font_file"), "%s: logged once", name)
	}
}

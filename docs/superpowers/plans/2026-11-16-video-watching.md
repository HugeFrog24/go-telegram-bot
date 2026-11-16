# Video Watching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a bot with a `video` config block "watch" videos, GIFs, round video messages, video files and video stickers, through ffmpeg-built timestamped sheets and a `view_frames` lookup tool with a per-turn budget.

**Architecture:**
- `ffmpeg.go` wraps ffprobe/ffmpeg (probe, allowlist, sizing rule, timestamped sheets and single frames).
- `video_store.go` keeps downloaded files and their records (refs `v1`, `v2`, … per chat, expiry, sweeper, `/clear_hard`).
- `video.go` turns incoming media into a stored message carrying sheet images and labels, through the existing photo turn path.
- `video_tool.go` defines `view_frames` and runs it inside an extended `getAnthropicResponse` loop.

Everything is gated on the bot's config, so bots without `video` behave exactly as today.

**Tech Stack:** Go 1.27.4, `anthropic-sdk-go/v2` v2.9.0 (beta Messages, client tools, Files API), `go-telegram/bot` v1.28.0, GORM + SQLite, ffmpeg/ffprobe via `os/exec`, testify.

**Spec:** `docs/superpowers/specs/2026-11-16-video-watching-design.md`

## Global Constraints

- **Dependencies:** none added to `go.mod`. ffmpeg and ffprobe are external binaries found with `exec.LookPath`.
- **Off means unchanged:** a bot whose config has no `video` block sends byte-identical API requests (no `view_frames` tool), makes the same Telegram calls, and stores the same rows as today.
- **Config defaults** (block `video`):

  | Key | Default | Bounds |
  |---|---|---|
  | `max_duration_seconds` | 600 | 1–3600 |
  | `sheet_frames` | 12 | 4–20 |
  | `max_lookups` | 6 | 1–20 |
  | `max_frames_per_lookup` | 12 | 1–20 |
  | `retention_hours` | 24 | 1–720 |
  | `font_file` | optional path | |

- **Download limit:** `maxVideoDownloadBytes = 20 * 1024 * 1024`. Larger files are never downloaded.
- **Image sizing rule (every image the bot builds):** long edge at most 1,568 px **and** `⌈w/28⌉ × ⌈h/28⌉` at most 1,568 visual tokens. Never upscale. Video-sticker sheets: long edge at most 512 px. Images are JPEG.
- **Sheets:**
  - Grid `gridFor(n)`: `cols = ⌈√n⌉`, `rows = ⌈n/cols⌉`, so 12 → 4×3 and 6 → 3×2.
  - Frame times for `n` frames over `[s, e]`: `t_i = s + i·(e−s)/n`, for i = 0…n−1.
  - Each tile shows its source timestamp, burned in on a dark box.
  - Video stickers always use `stickerSheetFrames = 6`.
- **Security research is binding:** Task 0 researches pitfalls, CVEs/RCEs and prior art for parsing untrusted video. The controller records which of its recommendations are adopted, as rulings in the ledger, before Task 2 is dispatched. Adopted recommendations are binding for Task 2 (ffmpeg layer) and Task 8 (Docker/CI), on top of the hardening line below.
- **ffmpeg hardening:** every ffmpeg/ffprobe call uses `-nostdin` and `-protocol_whitelist file`, takes only bot-generated paths, and has a timeout (probe 10 s, first sheet 60 s, lookup 20 s). Accept only ffprobe `format_name` values starting with `mov,mp4,m4a,3gp,3g2,mj2`, `matroska,webm` or `gif`, with at least one video stream. A gosec G204 on these calls is suppressed only with `// #nosec G204 -- fixed binary, generated arguments`.
- **Tool `view_frames`:**
  - Strict schema, all four fields required:
    - `video` (string);
    - `start` and `end` (number, seconds);
    - `count` (integer, 1…`max_frames_per_lookup`).
  - Sent only when the bot's video feature is on, on every request.
  - No `eager_input_streaming`, because the input is tiny. `tool_choice` stays the default `auto`.
- **Tool loop:** at most `maxToolSteps = 8` tool round trips per turn. Every `view_frames` call counts against `max_lookups`, including several in one response. Tool exchanges never enter chat memory or the database.
- **Model-facing label formats** (English; never sent to users):
  - **Kind names:** `Video`, `GIF`, `Round video`, `Video file`, `Video sticker`.
  - **Durations:** `m:ss`, or `h:mm:ss` from one hour.
  - **Sheet label:** `<Kind> v<N> (<dur>, <W>×<H>, <n> frames every <step> s; lookups until <YYYY-MM-DD HH:MM> UTC)`, with `<step>` to one decimal.
  - **Sticker label:** `Video sticker (<dur>, 6 frames; no lookups)`.
  - **Not watched:** `[<Kind> <dur>, <size> MB: not watched — <reason>]`, where `<reason>` is one of:
    - `over the 20 MB limit`;
    - `longer than the <max dur> limit`;
    - `could not be decoded`;
    - `could not be processed`.
    - Unknown parts are omitted.
  - **Lookup result:** `frame at <m:ss.s>, lookups left: <n>` or `frames at <m:ss.s>–<m:ss.s>, lookups left: <n>`.
  - **Lookup errors:**
    - `video <ref> not found in this chat`;
    - `video <ref> is no longer available`;
    - `lookup budget used up (<max> per turn); answer from what you have`;
    - `unknown tool <name>`;
    - `invalid input: <detail>`.
- **No new user-facing text:** the bot adds none. When it cannot watch something, the model answers in its own words.
- **Storage:** retained files live under `<videoRoot>/<botID>/<chatID>/<ref>.<ext>`, where `videoRoot` defaults to `data/videos`. Files are created with mode 0600. Expiry is measured with `b.clock`. `/clear_hard` deletes the video files and records linked to the deleted message rows. `/clear` leaves them to expire.
- **Checks, carried over:**
  - `go test -race -count=1 ./...`, `go vet ./...`, gosec v2.30.0 and golangci-lint v2.15.0 stay clean.
  - Tests needing ffmpeg skip with `t.Skip` when `exec.LookPath("ffmpeg")` fails. Tests never reach real Anthropic, Telegram or ElevenLabs APIs.
  - No `t.Parallel`.
  - A test name with underscores carries `//NOSONAR go:S100 -- underscore separation is idiomatic in Go test names`.
  - Never run `go build ./...`, which rewrites the tracked exe; use `go build -o "$TMPDIR/x.exe" .`.
  - Never read `config/*.json` except `config/default.json`.
- **Duplication check before every commit (Tasks 1–8), jscpd 5.4.3:**
  - **Gate, must exit 0:** `jscpd --format go --ignore "**/*_test.go" --similarity --baseline-from-ref HEAD --fail-on-new-clones -r console .`
  - **Semantic report, non-blocking:** `jscpd --format go --ignore "**/*_test.go" --semantic --semantic-scope same --kind semantic -r console .` Any pair involving the task's files is either merged or explained in the task report.
  - **Baseline at `d12ff83`:** 1 production clone (`config.go` loadConfig/Reload).
  - Capture exit codes without piping.
- **Commits:** directly on `main`. Stage by name and never stage `go-telegram-bot.exe`. Every message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Editing a video's caption** replaces only the caption. The sheet labels and the not-watched note survive, because they live outside `Message.Text`. Test: Task 4, `TestHandleEditedMessage_VideoCaptionKeepsLabels`.
2. **When a sheet's Files API ID dies and self-heal strips it,** the matching label goes too. The remaining labels stay aligned with their images. Test: Task 4, `TestStripDeadFileID_KeepsLabelsAligned`.
3. **Two videos from one album saved concurrently** get distinct refs, with no unique-index error. Test: Task 3, `TestSaveVideo_ConcurrentRefsAreDistinct`.
4. **A GIF whose Telegram duration is 0, or a one-frame GIF,** still gets a sheet: the duration comes from the probe, there's no divide-by-zero, and all frames are at t=0. Test: Task 2, `TestRenderSheet_SingleFrameGIF`.
5. **Tiny, square, portrait and landscape inputs** (a 240×240 round video, a 512×512 sticker, 1280×720, 720×1280) are never upscaled and always meet both size limits. Test: Task 2, `TestFitImageSize`.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `config.go` | `VideoConfig`, defaults, validation, advisory | 1 |
| `bot.go` | `Bot.video *VideoConfig` (nil = off), `videoOn()`, `videoRoot`, `videoRefMu`, sweeper start in `Start` | 1, 3 |
| `ffmpeg.go` (new) | probe and allowlist, `fitImageSize`, `gridFor`, `renderSheet`, font resolution | 2 |
| `models.go`, `database.go` | `Video` model + AutoMigrate; `Message.ImageLabels`, `Message.MediaNote` | 3, 4 |
| `video_store.go` (new) | save, find, link, sweep and delete videos | 3 |
| `anthropic_files.go` | `/clear_hard` hook; label-aligned dead-file stripping | 3, 4 |
| `bot.go` (`contentBlocksForMessage`, memory strip) | label rendering and alignment | 4 |
| `video.go` (new) | `watchableMedia`, preparing a video, mixed photo/video media turns, sticker sheets | 5, 6 |
| `handlers.go`, `album_buffer.go` | dispatch to the media turn; albums with videos; sticker sheet hook | 5, 6 |
| `video_tool.go` (new) | `viewFramesTool`, `lookupBudget`, `runClientTools`, `viewFrames` | 7 |
| `anthropic.go` | tool in `buildMessageParams`; tool loop in `getAnthropicResponse` | 7 |
| `Dockerfile`, `.github/workflows/go-ci.yaml`, `README.md` | ffmpeg + font in the image, ffmpeg in CI, docs | 8 |

Tests: `config_test.go`, `ffmpeg_test.go`, `video_store_test.go`, `video_test.go`, `video_tool_test.go`, plus updates to `bot_test.go`/`prompt_cache_test.go` and `anthropic_stream_test.go` (the stub gains request-body capture and a tool_use event helper).

---

### Task 0: Research: pitfalls, CVEs/RCEs and prior art for parsing untrusted video

No Go code changes. This task produces the evidence that Tasks 2 and 8 build on, and commits the approved spec and this plan.

**Files:** Create `docs/superpowers/research/2026-11-16-untrusted-video-parsing.md`. Commit `docs/superpowers/specs/2026-11-16-video-watching-design.md` and `docs/superpowers/plans/2026-11-16-video-watching.md`.

**Scope:** the inputs this bot will parse:
- MP4/MOV, Matroska/WebM, GIF and Telegram round videos;
- codecs H.264, HEVC, VP8/VP9 (with alpha), AV1 and GIF;
- ffmpeg/ffprobe 9.x locally, and whatever Alpine's `ffmpeg` package ships in the Docker image;
- the Go side shelling out with `os/exec`.

- [ ] **Step 1: CVE and advisory search.** Collect notable CVEs from 2022 to today for ffmpeg/libavformat/libavcodec and the demuxers and decoders above: ID, component, impact (RCE, memory corruption, DoS, info leak), CVSS where given, fixed version, and whether exploitation in the wild is known. Sources:
  - web search ("extended" mode for recent and niche results);
  - NVD (`https://services.nvd.nist.gov/rest/json/cves/2.0?keywordSearch=ffmpeg`, public JSON);
  - GitHub's public advisory database (`https://github.com/advisories?query=ffmpeg`, and advisories in projects that wrap ffmpeg);
  - ffmpeg's own security page (`https://ffmpeg.org/security.html`).
  - Record which ffmpeg version Alpine's current package ships, and whether the listed CVEs are fixed in it.
- [ ] **Step 2: Attack techniques against ffmpeg-as-a-service.** Document each with sources, and say whether `-protocol_whitelist file`, a format allowlist, or neither stops it:
  - playlist and protocol abuse (HLS/`concat`/`subfile`/`file:` references inside a crafted file, used for SSRF or local file reads);
  - polyglot files;
  - decompression and resource bombs (huge dimensions, frame counts, durations, pathological GIFs);
  - decoder crashes.
- [ ] **Step 3: Prior art.** Find how mature projects run ffmpeg on untrusted uploads. For each: flags used, format/codec allowlists, resource limits (`-threads`, `-timelimit`, memory/ulimit, cgroups), sandboxing (separate user, seccomp, containers), and update policy. Candidates:
  - Mastodon;
  - PeerTube;
  - Synapse / matrix-media-repo;
  - pict-rs;
  - Discourse;
  - Telegram bot frameworks;
  - imgproxy and Thumbor;
  - ImageMagick's `policy.xml`, as an analogue.

  Search GitHub broadly: web search with `site:github.com` (for example `"-protocol_whitelist" ffmpeg untrusted`), advisories in those repos, and their issue trackers. Code search requires a login and `gh` is unauthenticated here, so say so if it's unavailable instead of guessing.
- [ ] **Step 4: Recommendations.** End the document with a ranked table. Each row gives the recommendation, the threat it addresses, its cost or complexity here, and a target:
  - **Task 2:** ffmpeg flags, allowlists, limits;
  - **Task 8:** Docker image, user, package pinning, update policy;
  - **later;**
  - **decline,** with the reason.

  Recommendations must be concrete (exact flags or settings) and fit this bot: a single Go binary, ffmpeg via `os/exec`, an Alpine runtime image, files of 20 MB or less from Telegram.
- [ ] **Step 5: Verify the document.** Every factual claim has a source URL and access date. Every CVE row has an ID and a fixed version, or "unfixed / not applicable". The recommendation table covers every technique from Step 2.
- [ ] **Step 6: Commit.**
  - First `git add docs/superpowers/specs/2026-11-16-video-watching-design.md docs/superpowers/plans/2026-11-16-video-watching.md` → `docs: add video-watching design and plan`.
  - Then `git add docs/superpowers/research/2026-11-16-untrusted-video-parsing.md` → `docs: research on parsing untrusted video safely`.
- [ ] **Step 7 (controller): Rule on the recommendations** before dispatching Task 2. Record each adopt/decline as a ledger ruling, and carry the adopted ones into the Task 2 and Task 8 dispatches.

---

### Task 1: `video` config block and the on/off gate

**Files:** Modify `config.go`, `bot.go`. Test: `config_test.go`, `bot_test.go`.

**Interfaces:**
- Produces:
```go
type VideoConfig struct {
	MaxDurationSeconds int    `json:"max_duration_seconds,omitempty"`
	SheetFrames        int    `json:"sheet_frames,omitempty"`
	MaxLookups         int    `json:"max_lookups,omitempty"`
	MaxFramesPerLookup int    `json:"max_frames_per_lookup,omitempty"`
	RetentionHours     int    `json:"retention_hours,omitempty"`
	FontFile           string `json:"font_file,omitempty"`
}
// BotConfig gains: Video *VideoConfig `json:"video,omitempty"`
func (c VideoConfig) withDefaults() VideoConfig        // zero fields get the Global Constraints defaults
// Bot gains: video *VideoConfig (nil = feature off), set in NewBot
func (b *Bot) videoOn() bool                            // b.video != nil
```

- [ ] **Step 1: Write the failing tests**

```go
func TestValidateConfig_Video(t *testing.T) //NOSONAR ...
// absent → ok; {} → ok; each key at 0 → ok (default); at its upper bound → ok;
// max_duration_seconds 3601, sheet_frames 3 and 21, max_lookups 21, max_frames_per_lookup 21,
// retention_hours 721, any negative → error containing "'video."
func TestVideoConfig_WithDefaults(t *testing.T) //NOSONAR ...
// VideoConfig{}.withDefaults() == {600, 12, 6, 12, 24, ""}; set values are kept
func TestNewBot_VideoGate(t *testing.T) //NOSONAR ...
// config without Video → b.videoOn() false; with Video and ffmpeg+ffprobe on PATH → true
// (skip the "true" half when exec.LookPath("ffmpeg") fails); with Video and PATH="" (t.Setenv) → false
```

- [ ] **Step 2: Run them:** `go test -run 'TestValidateConfig_Video|TestVideoConfig_WithDefaults|TestNewBot_VideoGate' ./...`. Expected: compile failure, because `VideoConfig` is undefined.
- [ ] **Step 3: Implement.**
  - Validation errors are named `'video.<key>' …`.
  - In `NewBot`, when `config.Video != nil`, look up `ffmpeg` and `ffprobe`. If both are found, set `b.video` to the defaulted copy. Otherwise log `[<id>] video: ffmpeg/ffprobe not found on PATH; video watching is off` and leave it nil.
  - `logConfigAdvisories` logs, when `video` is set: `[<id>] video watching on: lookups are narrated only on models that write progress notes`.
- [ ] **Step 4: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: gate exit 0.
- [ ] **Step 6: Commit:** `git add config.go bot.go config_test.go bot_test.go`, then commit `feat: video config block and on/off gate`.

---

### Task 2: ffmpeg layer: probe, sizing and timestamped sheets

**Files:** Create `ffmpeg.go`, `ffmpeg_test.go`.

**Interfaces:**
- Consumes: `VideoConfig` (Task 1).
- Produces:
```go
const (
	maxImageEdge       = 1568
	maxImageTokens     = 1568
	stickerSheetFrames = 6
	stickerSheetEdge   = 512
	probeTimeout       = 10 * time.Second
	sheetTimeout       = 60 * time.Second
	lookupTimeout      = 20 * time.Second
)
type mediaProbe struct {
	Format   string  // ffprobe format_name
	Duration float64 // seconds; 0 when unknown
	Width, Height int
	HasAlpha bool    // VP9/WebM with alpha (video stickers)
}
var errUnsupportedMedia = errors.New("unsupported media")
func probeMedia(ctx context.Context, path string) (mediaProbe, error)       // allowlist + ≥1 video stream, else errUnsupportedMedia
func fitImageSize(w, h, maxEdge int) (int, int)                              // largest same-aspect size ≤ maxEdge and ≤ maxImageTokens; never above (w,h)
func gridFor(n int) (cols, rows int)
type sheetSpec struct {
	Start, End float64 // seconds; End ignored when Frames == 1
	Frames     int
	MaxEdge    int     // maxImageEdge, or stickerSheetEdge for stickers
	FontFile   string  // "" → resolveFontFile()
}
func renderSheet(ctx context.Context, path string, probe mediaProbe, spec sheetSpec) (jpeg []byte, frameTimes []float64, err error)
func resolveFontFile(configured string) string // configured if it exists; else the first existing of
// /usr/share/fonts/dejavu/DejaVuSans.ttf, /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf, C:\Windows\Fonts\arial.ttf; else ""
```
- **How `renderSheet` works** (my call: one ffmpeg run per call):
  - Input seek `-ss Start`, with `-copyts` so `drawtext=text='%{pts\:hms}'` shows source time.
  - Then `fps`, `scale`, `drawtext` and, when `Frames > 1`, `tile`.
  - Alpha inputs decode with `-c:v libvpx-vp9` and are overlaid on a gray background before scaling.
  - Tile size comes from `fitImageSize` applied to the whole grid. When the probe duration is 0, every frame time is 0.

- [ ] **Step 1: Write the failing tests.** Clips are generated with `ffmpeg -f lavfi -i testsrc2=…` into `t.TempDir()`, and each test skips without ffmpeg.

```go
func TestFitImageSize(t *testing.T) // Review Focus 5 — table:
// (1280,720,1568) → long edge ≤1568, ⌈w/28⌉⌈h/28⌉ ≤ 1568, aspect within 1%; (720,1280,1568) same;
// (240,240,1568) → (240,240) unchanged; (512,512,512) → (512,512); (4000,3000,1568) → tokens ≤1568
func TestGridFor(t *testing.T)                 // 12→(4,3), 6→(3,2), 4→(2,2), 9→(3,3), 20→(5,4), 1→(1,1)
func TestProbeMedia_AcceptsAllowedFormats(t *testing.T) //NOSONAR ... // mp4 and webm and gif clips → Format prefix allowed, Duration≈clip length, W/H right
func TestProbeMedia_RejectsOthers(t *testing.T)         //NOSONAR ... // a .txt file, a .wav (audio only), an .m3u8 playlist → errUnsupportedMedia
func TestRenderSheet_SizesAndTimes(t *testing.T)        //NOSONAR ... // 37 s 720×1280 clip, Frames 12 → decodable JPEG within both limits; frameTimes == t_i formula
func TestRenderSheet_SingleFrame(t *testing.T)          //NOSONAR ... // Frames 1, Start 12.5 → one frame JPEG within limits; frameTimes == [12.5]
func TestRenderSheet_SingleFrameGIF(t *testing.T)       //NOSONAR ... // Review Focus 4: 1-frame GIF, probe Duration 0 → sheet produced, all frameTimes 0
func TestRenderSheet_StickerAlpha(t *testing.T)         //NOSONAR ... // 3 s VP9 webm with alpha (yuva420p) → Frames 6, MaxEdge 512 → long edge ≤512
func TestRenderSheet_Timeout(t *testing.T)              //NOSONAR ... // already-cancelled ctx → error, returns promptly
```

- [ ] **Step 2: Run them:** `go test -run 'TestFitImageSize|TestGridFor|TestProbeMedia|TestRenderSheet' ./...`. Expected: compile failure.
- [ ] **Step 3: Implement `ffmpeg.go`** per the interfaces, the hardening line in Global Constraints, and every Task 0 recommendation the controller adopted for Task 2. Add a test for each adopted flag or limit that can be observed (for example a rejected format, or a capped resource). Decode JPEG dimensions with `image.DecodeConfig`; that's stdlib, so no new dependency.
- [ ] **Step 4: Run them with the race detector:** `go test -race -count=3 -run '…same…' ./...`. Expected: PASS.
- [ ] **Step 5: Run the full suite, then the duplication check.**
- [ ] **Step 6: Commit:** `git add ffmpeg.go ffmpeg_test.go`, then commit `feat: ffmpeg probe, sizing rule and timestamped sheets`.

---

### Task 3: Video records, storage, expiry and `/clear_hard`

**Files:** Create `video_store.go`, `video_store_test.go`. Modify `models.go`, `database.go`, `bot.go` (`videoRoot`, `videoRefMu`, sweeper in `Start`), `anthropic_files.go` (`deleteRowFiles`), the test helper `setupTestDB` (AutoMigrate `Video`).

**Interfaces:**
- Consumes: `mediaProbe` (Task 2), `b.video` (Task 1).
- Produces:
```go
type Video struct {
	gorm.Model
	BotID     uint   `gorm:"uniqueIndex:idx_video_ref"`
	ChatID    int64  `gorm:"uniqueIndex:idx_video_ref"`
	Ref       string `gorm:"uniqueIndex:idx_video_ref"` // "v1", "v2", … per bot+chat
	MessageID uint   `gorm:"index"`                     // stored Message row showing its sheet; 0 until linked
	Kind      string                                    // "video" | "animation" | "video_note" | "document"
	Path      string                                    // "" once the file is deleted
	Duration  float64
	Width, Height int
	SizeBytes int64
	ExpiresAt time.Time `gorm:"index"`
}
var (errVideoNotFound = errors.New("video not found"); errVideoExpired = errors.New("video expired"))
const videoSweepInterval = time.Hour
func (b *Bot) saveVideo(chatID int64, kind, ext string, data []byte, probe mediaProbe) (Video, error) // allocates Ref under videoRefMu
func (b *Bot) linkVideoToMessage(videoID, messageID uint) error
func (b *Bot) findVideo(chatID int64, ref string) (Video, error) // scoped to b.botID + chatID; expired or Path=="" → errVideoExpired
func (b *Bot) sweepExpiredVideos() (int, error)                  // deletes expired files, clears Path; returns count
func (b *Bot) deleteVideosForMessage(messageID uint) error        // removes files (missing file is fine) and hard-deletes records
```
- **Refs:** the next number is the count of all of this chat's records, including soft-deleted ones, plus 1. So refs are never reused in a chat.
- **`Start(ctx)`:** when `videoOn()`, runs the sweep once, then every `videoSweepInterval` until ctx is done.
- **`deleteRowFiles(row)`:** also calls `deleteVideosForMessage(row.ID)`, and returns false if that fails.

- [ ] **Step 1: Write the failing tests.** They use `setupBotForTest`, set `b.videoRoot = t.TempDir()` and a non-nil `b.video`, and use `MockClock`.

```go
func TestSaveVideo_RefsPerChat(t *testing.T)            //NOSONAR ... // chat A: v1, v2; chat B: v1; file exists at <root>/<botID>/<chat>/<ref>.mp4 with mode 0600 (skip mode check on Windows)
func TestSaveVideo_ConcurrentRefsAreDistinct(t *testing.T) //NOSONAR ... // Review Focus 3: 8 goroutines save into one chat → 8 distinct refs, no error
func TestFindVideo_Scoping(t *testing.T)                //NOSONAR ... // other chat's ref → errVideoNotFound; unknown ref → errVideoNotFound; own ref → found
func TestFindVideo_Expired(t *testing.T)                //NOSONAR ... // clock advanced past RetentionHours → errVideoExpired
func TestSweepExpiredVideos(t *testing.T)               //NOSONAR ... // expired file removed, Path cleared, count 1; fresh one untouched
func TestClearHard_DeletesLinkedVideos(t *testing.T)    //NOSONAR ... // message row linked to a video → /clear_hard path (hardDeleteScope) removes file and record
```

- [ ] **Step 2: Run them:** `go test -run 'TestSaveVideo|TestFindVideo|TestSweepExpiredVideos|TestClearHard_DeletesLinkedVideos' ./...`. Expected: compile failure.
- [ ] **Step 3: Implement** per the interfaces. AutoMigrate `&Video{}` in `initDB` and in `setupTestDB`.
- [ ] **Step 4: Run the full suite, then the duplication check.** Expected: PASS and gate exit 0.
- [ ] **Step 5: Commit:** `git add video_store.go video_store_test.go models.go database.go bot.go anthropic_files.go handlers_test.go`, then commit `feat: video records, retention and clear_hard cleanup`.

---

### Task 4: Per-image labels and media notes on stored messages

**Files:** Modify `models.go` (Message), `bot.go` (`contentBlocksForMessage`, `stripDeadFileIDFromMemory`), `anthropic_files.go` (`stripDeadFileIDs` and its callers). Test: `bot_test.go` (or `prompt_cache_test.go`), `edit_stop_test.go`.

**Interfaces:**
- Produces:
```go
// Message gains:
ImageLabels []string `gorm:"type:text;serializer:json"` // parallel to ImageFileIDs; "" = default label
MediaNote   string   `gorm:"type:text"`                 // factual note on media the bot could not watch
// contentBlocksForMessage: for image i → a text block ImageLabels[i] if non-empty,
// else "Image i+1:" when there are several images; then the image.
// After the images: MediaNote as a text block if set; then Text.
func stripDeadFileIDs(src, labels []string, deadSet map[string]struct{}) (survivors, survivorLabels []string, dirty bool)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestContentBlocksForMessage_Labels(t *testing.T) //NOSONAR ...
// one image with label "Video v1 (0:37, …)" → [text label, image, text caption];
// two images, labels ["", "GIF v2 (…)"] → [text "Image 1:", image, text "GIF v2 (…)", image, …];
// no images, MediaNote "[Video 2:13, 48 MB: not watched — over the 20 MB limit]" + Text "look" → [note, "look"];
// rows without labels/note → exactly today's blocks
func TestStripDeadFileID_KeepsLabelsAligned(t *testing.T) //NOSONAR ... // Review Focus 2: ids [a,b,c], labels ["", "L-b", "L-c"], dead b → ids [a,c], labels ["", "L-c"] (DB path and memory path)
func TestHandleEditedMessage_VideoCaptionKeepsLabels(t *testing.T) //NOSONAR ... // Review Focus 1: stored row with ImageLabels + MediaNote, caption edit → Text replaced, labels and note unchanged
```

- [ ] **Step 2: Run them:** `go test -run 'TestContentBlocksForMessage_Labels|TestStripDeadFileID_KeepsLabelsAligned|TestHandleEditedMessage_VideoCaptionKeepsLabels' ./...`. Expected: FAIL or compile failure.
- [ ] **Step 3: Implement** per the interfaces. Every caller of `stripDeadFileIDs` keeps labels aligned, and existing rows with nil labels behave as today.
- [ ] **Step 4: Run the full suite, then the duplication check.**
- [ ] **Step 5: Commit:** `git add models.go bot.go anthropic_files.go bot_test.go edit_stop_test.go` (adjust to the files you touched), then commit `feat: per-image labels and media notes on messages`.

---

### Task 5: Watching a single video, GIF, round video or video file

**Files:** Create `video.go`, `video_test.go`. Modify `handlers.go` (dispatch; `handlePhotoMessage` generalized) and `anthropic_stream_test.go`.

**Test helpers:**
- `MockTelegramClient` `GetFileFunc`/`FileDownloadLinkFunc`, plus an `httptest` file server for downloads.
- The Anthropic stub server, extended in this task:
  - it answers `POST /v1/files` with `{"id":"file_…"}`;
  - it records each `/v1/messages` request body, readable by the test. Task 7 reuses this.

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces:
```go
const maxVideoDownloadBytes = 20 * 1024 * 1024
type watchable struct {
	Kind      string // "video" | "animation" | "video_note" | "document" | "sticker"
	FileID    string
	FileSize  int64  // 0 when unknown
	Duration  int    // Telegram seconds; 0 when unknown
	Ext       string // "mp4", "webm", "gif" (from mime/kind)
}
func watchableMedia(msg *models.Message) (watchable, bool)
// video, animation, video_note, document with video/* or image/gif mime, sticker with IsVideo (Kind "sticker")
type preparedMedia struct {
	ImageFileID string // "" when not watched
	Label       string // sheet label, or "" when not watched
	Note        string // not-watched note, or ""
	VideoID     uint   // record to link after the message is stored; 0 for photos/stickers/unwatched
}
func (b *Bot) prepareVideo(ctx context.Context, chatID int64, tgMessageID int, w watchable) preparedMedia
func (b *Bot) handleMediaMessage(ctx context.Context, items []*models.Message, to replyTarget, userID int64,
	username, firstName, lastName string, isPremium bool, languageCode string, messageTime int)
// replaces handlePhotoMessage: items may mix photos and watchable videos; one stored Message with
// ImageFileIDs/ImageLabels in item order, MediaNote = notes joined by "\n", Text = album caption;
// after storing, linkVideoToMessage for each VideoID; then the same runTurn call as photos today.
```
- **How `prepareVideo` works:**
  - **Before downloading:** if `FileSize > maxVideoDownloadBytes`, return a note "over the 20 MB limit". If the Telegram duration is over the limit, return "longer than … limit".
  - **Otherwise:**
    1. Download with `downloadTelegramFile` into a temp file.
    2. `probeMedia`; on `errUnsupportedMedia`, note "could not be decoded".
    3. Re-check the probed duration.
    4. `saveVideo`.
    5. `renderSheet` over `[0, duration]` with `SheetFrames`.
    6. `uploadImageToAnthropic`.
    7. Build the label.
  - **Errors:** any other failure, including a timeout, gives the note "could not be processed". A failed step deletes the saved file and record and logs the cause.
  - **Concurrency:** videos in one call are prepared in parallel with photos, like today's errgroup. A failed photo upload keeps today's behavior.
- **`handleUpdate` dispatch:** when `b.videoOn()` and `watchableMedia` returns a non-sticker kind for a message outside a media group: `cancelIntake`, rate-limit check, `handleMediaMessage(ctx, []*models.Message{message}, …)`.
- **With the feature off:** today's code path, unchanged.

- [ ] **Step 1: Write the failing tests** (ffmpeg-backed ones skip without ffmpeg)

```go
func TestWatchableMedia(t *testing.T) // table: Video, Animation(+Document), VideoNote, Document video/mp4, Document image/gif → ok with Kind;
// Document application/pdf, Photo, Voice, Sticker non-video → not ok; Sticker IsVideo → Kind "sticker"
func TestHandleMediaMessage_VideoStoresSheetAndLabel(t *testing.T) //NOSONAR ...
// video on, 5 s clip served via mocked Telegram download, Files API stub → stored Message: ImageFileIDs [file_…],
// ImageLabels ["Video v1 (0:05, 720×1280, 12 frames every 0.4 s; lookups until … UTC)"], Text = caption,
// Video record linked to the row; the turn's request to Anthropic carries [label, image, caption]
func TestHandleMediaMessage_OverLimitNotDownloaded(t *testing.T) //NOSONAR ...
// FileSize 25 MB → GetFile never called; MediaNote "[Video 2:13, 25 MB: not watched — over the 20 MB limit]"; no Telegram text sent by the bot itself
func TestHandleMediaMessage_TooLong(t *testing.T)   //NOSONAR ... // max_duration_seconds 3, 5 s clip → note "… longer than the 0:03 limit]"
func TestHandleMediaMessage_Undecodable(t *testing.T) //NOSONAR ... // served bytes are text → note "… could not be decoded]", no video record left
func TestHandleUpdate_VideoOffUnchanged(t *testing.T) //NOSONAR ... // video off: a video update stores the same empty-text row as before and starts no turn
```

- [ ] **Step 2: Run them:** `go test -run 'TestWatchableMedia|TestHandleMediaMessage|TestHandleUpdate_VideoOffUnchanged' ./...`. Expected: compile failure.
- [ ] **Step 3: Implement** per the interfaces. Photo-only behavior stays identical: run the existing photo tests unchanged.
- [ ] **Step 4: Run the full suite, then the duplication check.**
- [ ] **Step 5: Commit:** `git add video.go video_test.go handlers.go` (plus any helpers touched), then commit `feat: watch videos, GIFs and round videos through timestamped sheets`.

---

### Task 6: Albums with videos, and video stickers

**Files:** Modify `album_buffer.go`, `handlers.go` (sticker branch), `video.go`. Test: `video_test.go`.

**Interfaces:**
- Consumes: `watchableMedia`, `handleMediaMessage`, `renderSheet`, `fitImageSize` (Tasks 2 and 5).
- Produces:
```go
// bufferAlbumItem accepts items with Photo, or (when b.videoOn()) a non-sticker watchable; flushAlbum calls handleMediaMessage.
func (b *Bot) attachStickerSheet(ctx context.Context, chatID int64, userMsg *Message, msg *models.Message)
// video on + Sticker.IsVideo: download → temp file → probe → renderSheet(Frames stickerSheetFrames, MaxEdge stickerSheetEdge)
// → upload → set ImageFileIDs/ImageLabels ["Video sticker (0:03, 6 frames; no lookups)"] on the stored row (DB + memory);
// on failure set MediaNote "[Video sticker 0:03: not watched — could not be processed]"; the temp file is always removed; no Video record.
```
- **Sticker branch in `handleUpdate`:** after `beginTurn` and before `snapshotForTurn`, call `attachStickerSheet` when `b.videoOn() && message.Sticker.IsVideo`. Everything else in the sticker path is unchanged, including the emoji text, `respond_with_emojis` and the existing fallbacks.

- [ ] **Step 1: Write the failing tests**

```go
func TestAlbum_PhotoAndVideoBecomeOneTurn(t *testing.T) //NOSONAR ... // 1 photo + 2 videos in one MediaGroupID → one stored Message, 3 images, labels ["", "Video v1 …", "Video v2 …"], distinct refs, one Anthropic request
func TestAlbum_VideoOffKeepsTodaysBehavior(t *testing.T) //NOSONAR ... // video off: video items in a group are not buffered (as today)
func TestAttachStickerSheet(t *testing.T)               //NOSONAR ... // 3 s alpha webm sticker → stored sticker row keeps Text "Sent a sticker: 😀", gains one image ≤512 px and the sticker label; no Video record; temp file gone
func TestAttachStickerSheet_FailureNote(t *testing.T)   //NOSONAR ... // undecodable bytes → MediaNote set, no image, reply still produced through the sticker path
```

- [ ] **Step 2: Run them.** Expected: FAIL or compile failure.
- [ ] **Step 3: Implement** per the interfaces.
- [ ] **Step 4: Run the full suite, then the duplication check.**
- [ ] **Step 5: Commit:** `git add album_buffer.go handlers.go video.go video_test.go`, then commit `feat: albums with videos and video sticker sheets`.

---

### Task 7: The `view_frames` tool and the client-tool loop

**Files:** Create `video_tool.go`, `video_tool_test.go`. Modify `anthropic.go` (`buildMessageParams`, `getAnthropicResponse`) and `anthropic_stream_test.go` (new helper `evToolUseBlock(index int, id, name, inputJSON string) []string`; the request-body capture comes from Task 5).

**Interfaces:**
- Consumes: `findVideo`, `renderSheet`, `fitImageSize`, `b.video` (Tasks 1–3).
- Produces:
```go
const (viewFramesToolName = "view_frames"; maxToolSteps = 8)
func viewFramesTool(cfg VideoConfig) anthropic.BetaToolUnionParam // strict; schema per Global Constraints; count maximum = cfg.MaxFramesPerLookup
type lookupBudget struct{ max, used int }
type viewFramesInput struct { Video string `json:"video"`; Start, End float64; Count int `json:"count"` }
func (b *Bot) viewFrames(ctx context.Context, chatID int64, in viewFramesInput, budget *lookupBudget) (text string, jpeg []byte, err error)
func (b *Bot) runClientTools(ctx context.Context, chatID int64, msg anthropic.BetaMessage, budget *lookupBudget) []anthropic.BetaContentBlockParamUnion
// one tool_result per tool_use block, in order; success → [text, base64 image/jpeg]; failure → is_error with the error text
```
- **The tool description** (English, model-facing; the builder writes the wording) must say:
  - `count` 1 returns one full-detail frame at `start`, and `end` is ignored;
  - `count` above 1 returns a timestamped sheet of `count` frames spread over `[start, end]`;
  - times are in seconds;
  - `video` is the ref from a video's label (e.g. `v3`);
  - every result reports the lookups left this turn.
- **`buildMessageParams`:** appends `viewFramesTool(*b.video)` after the web tools when `b.videoOn()`.
- **`getAnthropicResponse` on `StopReason == tool_use`:**
  1. Increment the step counter. Past `maxToolSteps`, log, `a.flushHeld()` and break.
  2. Append `msg.ToParam()`, then `anthropic.NewBetaUserMessage(runClientTools(…)...)`.
  3. Continue. One `lookupBudget{max: b.video.MaxLookups}` is created per call.
- **`viewFrames`:**
  - Clamps `start`/`end` to `[0, Duration]` and swaps them if reversed.
  - `count` outside `1…MaxFramesPerLookup` gives an `invalid input` error, though the strict schema should prevent it.
  - Every call counts against the budget, whether it succeeds or fails.
  - On success it re-probes the stored file with `probeMedia`, because the record does not keep the format or alpha flag. It then runs `renderSheet` with `lookupTimeout`.

- [ ] **Step 1: Write the failing tests** (the stub serves scripted SSE; an ffmpeg-generated clip is saved with `saveVideo`)

```go
func TestBuildMessageParams_ViewFramesTool(t *testing.T) //NOSONAR ... // video off → no tool named view_frames (Tools nil when nothing else); on → present, strict, required [video,start,end,count], count max = cfg value
func TestGetAnthropicResponse_ViewFramesLoop(t *testing.T) //NOSONAR ...
// stream 1: tool_use view_frames {"video":"v1","start":1,"end":1,"count":1}, stop tool_use; stream 2: text "It's a cat.", end_turn
// → reply "It's a cat."; request 2 body has a tool_result for that tool_use_id with text "frame at 0:01.0, lookups left: 5"
//   and an image/jpeg base64 whose decoded size meets both limits; chat memory unchanged
func TestViewFrames_BudgetCountsEveryCall(t *testing.T) //NOSONAR ... // max_lookups 2, one response with 3 tool_use blocks → results 1-2 ok, 3 is_error "lookup budget used up (2 per turn); answer from what you have"
func TestViewFrames_ScopingAndExpiry(t *testing.T)      //NOSONAR ... // other chat's ref → "video v1 not found in this chat"; expired → "video v1 is no longer available"; both is_error
func TestRunClientTools_UnknownTool(t *testing.T)       //NOSONAR ... // tool_use "other" → is_error "unknown tool other"
func TestGetAnthropicResponse_ToolStepCap(t *testing.T) //NOSONAR ... // stub always answers tool_use → exactly maxToolSteps+1 requests, then the loop stops
```

- [ ] **Step 2: Run them:** `go test -run 'TestBuildMessageParams_ViewFramesTool|TestGetAnthropicResponse_ViewFramesLoop|TestViewFrames|TestRunClientTools|TestGetAnthropicResponse_ToolStepCap' ./...`. Expected: compile failure.
- [ ] **Step 3: Implement** per the interfaces. Look up exact SDK type names in the local anthropic-sdk-go v2.9.0 source (`BetaToolParam`, `BetaToolInputSchemaParam`, the tool-result block constructor and its content union).
- [ ] **Step 4: Run the full suite, then the duplication check.**
- [ ] **Step 5: Commit:** `git add video_tool.go video_tool_test.go anthropic.go anthropic_stream_test.go`, then commit `feat: view_frames lookups through a client-tool loop`.

---

### Task 8: Docker, CI and README

**Files:** Modify `Dockerfile`, `.github/workflows/go-ci.yaml`, `README.md`.

- [ ] **Step 1: Docker image.** Add `ffmpeg font-dejavu` to the runtime stage's `apk add`, and apply every Task 0 recommendation the controller adopted for Task 8 (for example a pinned or minimum ffmpeg version, or a non-root user, which already exists).
  - Verify: `docker build -t tgbot-video-check .`.
  - Then `docker run --rm --entrypoint sh tgbot-video-check -c "ffmpeg -hide_banner -filters | grep -q drawtext && test -f /usr/share/fonts/dejavu/DejaVuSans.ttf && echo OK"` must print `OK`.
  - If the font path differs, update `resolveFontFile`'s candidate list (Task 2) in this task.
- [ ] **Step 2: CI.** In the `test` job, add a step before `go test`: `sudo apt-get update && sudo apt-get install -y ffmpeg fonts-dejavu-core`. The ffmpeg-backed tests then run in CI instead of skipping.
- [ ] **Step 3: README.**
  - Add a **Video watching** section after **Progress notes**. It covers:
    - the config block and the defaults table;
    - which messages are watched, and that `.tgs` stickers aren't;
    - the sheet and lookups, with lookups narrated only on models that write progress notes;
    - cost: one sheet stays in history per video, plus up to `max_lookups` images per turn;
    - retention, and `/clear_hard` versus `/clear`;
    - the ffmpeg, ffprobe and font requirement for native deployments;
    - an optional `custom_instructions` line: "When a question is about a specific moment in a video, look at it with view_frames before answering."
  - Add the `video` row to the configuration table.
- [ ] **Step 4: Run the full suite, then the duplication check.**
- [ ] **Step 5: Commit:** `git add Dockerfile .github/workflows/go-ci.yaml README.md ffmpeg.go`, then commit `chore: ffmpeg in the image and CI; document video watching`.

---

### Task 9: Full verification and live check

- [ ] **Step 1: Run the checks:** `go build -o "$TMPDIR/v.exe" . && go vet ./... && go test -race -count=1 ./...`. Expected: all pass, with ffmpeg tests running locally because ffmpeg 9.0.2 is installed.
- [ ] **Step 2: Run the linters:** `gosec ./...` and `golangci-lint run --timeout 5m`, from `$(go env GOPATH)/bin`. Expected: 0 issues. Fix findings in feature code only, then commit `fix: address lint findings`.
- [ ] **Step 3: Final duplication report** against `d12ff83`:
  - Gate: `--baseline-from-ref d12ff83`, which must exit 0.
  - The semantic report.
  - The tests-included information run.
  - Put all three outputs in the report.
- [ ] **Step 4: Live check (owner, after deploying).** Add `"video": {}` to a test bot's config.
  - Send it, in a private chat and in a Business chat:
    - a short phone video with a caption;
    - a GIF;
    - a round video message;
    - a video sticker;
    - an album with a photo and a video;
    - a video over 20 MB.
  - Expected:
    - sensible answers that mention what's in the clip;
    - a follow-up "what happens at 0:12?" answered through a lookup, with a progress note on Sonnet/Opus 5.5;
    - the over-limit video answered in the model's own words;
    - `/clear_hard` removes the files under `data/videos/…`.

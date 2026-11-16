# Security decisions for video watching

Date: 2026-11-16. These are the controller's rulings on Task 0, and they are **binding for Tasks 2, 3, 5, 7 and 8** of `docs/superpowers/plans/2026-11-16-video-watching.md`. Where they differ from the plan's Global Constraints hardening line, this file wins.

**Inputs:**
- `2026-11-16-untrusted-video-parsing.md`: the research, after its fix round.
- Its task review and re-review.
- A blind cross-check by an independent agent that did not see the research first: `2026-11-16-untrusted-video-parsing-crosscheck-phase1.md` (its own list) and `…-phase2.md` (the comparison).

Numbers like "#5" refer to rows of the research's recommendation table.

## Task 2: ffmpeg layer

1. **Argument order and stdin.**
   - Every input option goes before `-i`.
   - ffprobe has no `-nostdin`, so `cmd.Stdin` stays nil. ffmpeg gets `-nostdin`.
   - Both get `-hide_banner -loglevel error`. (#3)
2. **Input naming.**
   - The input is `"file:" + absolute path`.
   - Downloads are saved with no extension. After the probe, the extension comes from `extForFormat(format_name)`: `mov,…` → `mp4`, `matroska,webm` → `webm`, `gif` → `gif`.
   - Never take an extension from Telegram's `file_path` or the user's `file_name`. (#4)
3. **Format allowlist on every call.** Every call passes `-protocol_whitelist file -format_whitelist <list>`. The list depends on the kind:

   | Kind | List |
   |---|---|
   | video sticker | `matroska` |
   | animation | `mov,gif` |
   | video and video_note | `mov,matroska` |
   | document | `mov,matroska,gif` |

   Keep the `format_name` prefix check as a second line. (#1; narrower per-kind lists from the cross-check)
4. **Decoder allowlists.**
   - **Probe:** the research §6.1 revised probe list **without `mjpeg,png`** (see "Cover art" below).
   - **Render:** `h264,hevc,vp8,vp9,libvpx,libvpx-vp9,libdav1d,libaom-av1,gif`, with `-map 0:V:0 -an -sn -dn`.
   - **Go-side stream choice:** the chosen stream is the first stream with `codec_type=video` and `disposition.attached_pic=0`, and its `codec_name` must be in {h264, hevc, vp8, vp9, av1, gif}. Attached pictures never count for codec, dimension or duration checks. (#2, §6.1, re-review)
5. **Pixel caps.**
   - Decoder side: `-max_pixels 16777216` on both tools.
   - Probe side: reject when width or height > 8192 or width × height > 16,777,216.
   - This replaces #5's 4096 / 8,294,400 caps, which the cross-check showed reject DCI 4K, GoPro 4000×3000/5.3K and 5K screen recordings.
6. **Thread and stream caps.**
   - `-threads 2` (input) and `-filter_threads 1`. (#6)
   - `-max_streams 32` on both tools. If any normal test clip fails with it, drop it and say so in the task report. (Cross-check)
7. **Output.**
   - Render with `-frames:v 1 -f image2pipe -c:v mjpeg pipe:1`.
   - Read stdout up to 8 MiB + 1 byte; reaching the extra byte means error and kill. Keep the last 64 KiB of stderr for logs.
   - **Failure is a non-zero exit or a signal, never stderr content**, because normal files print "Codec (aac) not on whitelist".
   - `cmd.WaitDelay = 2 s`. No retries. (#7, review minors)
8. **Concurrency.** One process-wide semaphore, capacity `max(1, min(4, runtime.NumCPU()/2))`.
   - Acquire it per process run, using that run's timeout context.
   - **Never hold it across two runs:** no nesting, and no acquiring at the album-item level. (#7b, re-review)
9. **Child environment.**
   - `cmd.Env` holds only `PATH`, plus `SYSTEMROOT` on Windows.
   - `cmd.Dir` is a directory that **contains only the input file**: a per-job temp directory for downloads, or the per-video directory for retained files (Task 3). (#8; cross-check)
10. **Orphans and runaways.**
    - On Linux (build-tagged): `SysProcAttr.Pdeathsig = SIGKILL`.
    - ffmpeg also gets `-timelimit <2 × the call's timeout in seconds>` as a CPU-time backstop where the binary accepts it. Verify; ffprobe may not. This reverses #20's decline, per the cross-check.
11. **Filtergraph and options built only from constants and Go-formatted finite numbers.**
    - Every number (rates, sizes, times) is checked finite (no NaN or ±Inf) and in range before it is formatted.
    - `drawtext` text is only `%{pts\:hms}`.
    - **Never** use `textfile=`, `movie=`, `amovie=`, or any string derived from a file, metadata, the user or the model in a filtergraph. The cross-check verified `drawtext=textfile=` and `movie=` read local files, and `movie=` ignores the input's whitelists.
    - The font path is escaped at option level and again at graph level; test with a Windows path such as `C:\Windows\Fonts\arial.ttf`.
    - **Never** `-hwaccel`, `-enable_drefs 1`, `-use_absolute_path 1`, `-safe 0`, `-stream_loop` or `-ignore_loop 0`. (#9, cross-check)
12. **One argument builder** serves probe, sheet and lookup, and render calls pin `-f` from the probe result. (#10)
13. **Tests.**
    - Extensionless **DASH MPD** and **ffconcat** fixtures fail with "Format not on whitelist" (non-zero exit). An HLS fixture is not enough: the HLS demuxer refuses non-`.m3u8` names on its own.
    - One test for each adopted flag or limit that can be observed.

## Task 3: storage

- Layout: `<videoRoot>/<botID>/<chatID>/<ref>/video.<ext>`. Each video gets its own directory, so `cmd.Dir` holds exactly one file.
- Directories 0700, files 0600. `deleteVideosForMessage` removes the per-video directory.

## Task 5: download and intake

- **Fix `downloadTelegramFile`,** which photos and voice also use:
  - its errors must never contain the request URL, because the bot token is in it; strip or redact it, for example by unwrapping `*url.Error` to its inner error;
  - use an HTTP client with a timeout (60 s) as well as the context;
  - read at most `limit + 1` bytes and error when that is reached (videos use `maxVideoDownloadBytes`; photos and voice get a sane bound, 20 MB).
  - Tests: an error string never contains the token, and an oversize body errors. (Cross-check: today a network error logs the token at `handlers.go:210`.)
- Downloads go to a per-job temp directory under an extensionless name, deleted after the job.
- **Labels never include ffprobe tags** (title, comment, encoder and so on): only the kind name and Go-formatted numbers. (Cross-check: prompt injection through metadata)

## Task 7: the tool

- `view_frames` validates numbers before use:
  - `start` and `end` must be finite and ≥ 0 before clamping;
  - `count` must be an integer in range.
  - Anything else returns `is_error` `invalid input: <detail>` and counts against the budget.
- Tests cover NaN, ±Inf and negative values, besides the scoping, expiry and budget tests in the plan.
- Text inside frames can prompt-inject the model. The defence is structural: the only tool is chat-scoped and budgeted, and a reply goes only to the same chat.

## Task 8: Docker, CI and operations

- **Image:** `apk add --no-cache 'ffmpeg>=8.1.3' font-dejavu`.
  - Keep `FROM alpine:latest` (it tracks new branches; the floor guards against old ffmpeg).
  - README: rebuild and redeploy at least monthly and after FFmpeg security releases. (#11, #12; the cross-check confirms 8.1.3 carries the fixes the research marked unverified.)
- **Image check:** `! ffmpeg -hide_banner -buildconf | grep -q -- --enable-avisynth`; print `ffmpeg -version | head -1`; confirm `drawtext` and the font file. (#13 with the corrected snippet)
- **`docker-compose.yml`:** `cap_drop: [ALL]`, `security_opt: ["no-new-privileges:true"]`, `pids_limit: 256`.
  - These are low-risk and adopted, because the bot needs no capabilities and already runs as non-root.
  - `mem_limit` and `read_only` stay README recommendations only, because they depend on the deployment. (#14, partly)
- **`examples/systemd/telegram-bot.service`:** `NoNewPrivileges=yes`, `PrivateTmp=yes`, `TasksMax=256`, `LimitCORE=0`. No `ProtectHome`, because the example's `WorkingDirectory` is under `/home`. (Cross-check)
- **`main.go`, Linux build tag:** `prctl(PR_SET_DUMPABLE, 0)` at startup through `syscall`, with no new dependency. Same-UID child processes (ffmpeg) then cannot read the bot's `/proc/<pid>/environ` or memory. (Cross-check)
- These files lie outside Task 8's original file list; this ruling allows them.

## Cover art

`png` and `mjpeg` stay **off** the probe list, so files with cover art become "could not be decoded".
- **Why:** the one applicable decoder record without hwaccel, CVE-2026-125180 (quadratic EXIF memory, open through 9.0.2), is bounded only by a memory limit, which is not adopted. The `mjpeg` decoder also decodes JPEG-LS (CVE-2026-125243, per the cross-check).
- **Revisit** once a per-process or container memory bound exists.

## Later

- A Landlock-restricted helper (#16).
- Per-child rlimits beyond `Pdeathsig` (#17).
- A minimal custom ffmpeg build (#18).
- `fd:` input (#19).
- `-max_alloc`.
- Dependabot (#12's repo-automation part).
- Compose `mem_limit` / `read_only`.

## Declined

- #21: stdin input.
- #22: `-count_frames`.
- #23: re-encoding uploads.
- #24: `-err_detect explode`.
- #25: running ffmpeg under another UID via `Credential`.
- (#20 `-timelimit` is now adopted; see Task 2 item 10.)

## Addendum: blind review of Task 1 (Ruling 9)

The blind security review of Task 1 (the config block and the on/off gate) raised these points. They are binding like the rest of this file.

- **Task 2: absolute tool paths.**
  - `ffmpeg.go` gets `lookupTool(name string) (string, error)`: `exec.LookPath`, then an error unless `filepath.IsAbs(result)`. An `exec.ErrDot` result, or a relative one under `GODEBUG=execerrdot=0`, is an error.
  - Every run uses the absolute path that `lookupTool` returns. Resolve it per run, with no caching, so tests that set PATH keep working.
  - `resolveVideo` in `bot.go` switches to `lookupTool`.
  - Also add a field comment on `Bot.video`: it is the only switch for the feature, and later code must never test `b.config.Video`.
- **Task 2: decode window.**
  - `renderSheet` passes an input `-t <window>` before `-i`. For `Frames > 1` the window is `End − Start` plus one frame step; for `Frames == 1` it is a small constant, such as 1 s.
  - This bounds the decode work even when the container's duration is a lie. The wall-clock timeouts, the semaphore and the pixel caps from Task 2 items 5–10 stay as they are.
- **Task 2: font file.** `resolveFontFile` accepts the configured path only when it is absolute and names a regular file. Otherwise it logs once and falls back to the built-in candidates. The path is still escaped as item 11 says, and it is never shown to users.
- **Task 3: total-size cap.**
  - The video store gets a per-bot cap, `maxVideoStoreBytes = 2 GiB`, as a constant.
  - After each save, the oldest videos are deleted first until the total is under the cap. Their records then read as expired, the same path as retention.
  - Task 8's README states the cap.
  - **Why:** retention limits age only, and the rate limiter is per user, so many senders could fill the data volume and break the SQLite database next to it.
- **Declined:**
  - A per-turn frame budget. Each lookup returns one tiled image, and `max_lookups` ≤ 20 plus `maxToolSteps` bound the number of calls.
  - An owner-only mode. The feature is already opt-in per bot.
  - A lower maximum for `retention_hours`. The size cap bounds the disk.
  - Naming the missing tool in the gate log. The log string is set by the plan.

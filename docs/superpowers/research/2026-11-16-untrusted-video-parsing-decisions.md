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

## Amendment: lookback for the decode window (Ruling 10)

This replaces the "Task 2: decode window" item of Ruling 9. The Task 2 review found that an accurate seek to `Start` drops the frame on screen at `Start` whenever that frame began earlier. On sparse, variable-frame-rate or still-image content, a sheet or a single frame then shows a *later* frame labelled with the requested time, or the render fails.

- **The new window:**
  - `renderSheet` seeks with `-ss max(0, Start − L)` and passes an input `-t W + L`, where `L = 10 s` is a constant.
  - `W` is as before: `End − Start` plus one step, or 1 s for one frame.
  - Times are still measured from `Start`, and the existing fps chain picks the frame on screen at each `t_i`.
  - `Frames == 1` also goes through the fps chain, and `-frames:v 1` stops it.
- **The cost:** at most `L` more seconds of decoding per call. The wall-clock timeouts, the semaphore and the pixel caps still bound the work.
- **The limit:**
  - A frame that began more than `L` before `Start` still yields no image.
  - Such a failure is reported as "no frame near that time", kept separate from a decode failure where ffmpeg can tell the two apart.
  - Otherwise it is the render-failure path.
- **Tests:**
  - A fixture made of time segments in distinct solid colours, plus a sparse variant with a frame every 35 s. For a sheet, a lookup and a single frame, tile k must show the colour of the segment that contains `frameTimes[k]`.
  - A still-image clip lookup at Start = 7 s must succeed.

## Isolation tests (Ruling 11)

Video lookups are scoped to bot and chat. The chat ID comes from the Telegram update, never from the model. The model supplies only a ref string, and file paths come from the stored record. The plan already tests cross-chat lookups. These tests are added, and they are binding:

- **Task 3:**
  - `TestFindVideo_OtherBotSameChat`: two bot IDs with the **same** chat ID. That is the common case, because one user messaging two bots privately has the same chat ID with both. Check three things:
    - `findVideo` returns `errVideoNotFound` across bots;
    - ref numbering is scoped to bot + chat, so each bot's first video is `v1`;
    - files land under different `<botID>` directories.
  - **Hostile refs:** `"../v1"`, `"v1/../../x"`, `"v1' OR 1=1 --"`, `""`, `"V1"`, `" v1"` and a 10 KB string each return `errVideoNotFound`. The ref is never used to build a path; check it as a string against `^v[1-9][0-9]{0,8}$` before any lookup.
- **Task 7:** an end-to-end loop test. The scripted model calls `view_frames` with a ref that exists only in another chat, and then one that exists only for another bot in the same chat. Each call returns `is_error` "… not found in this chat". The next request's body carries no image block, no frame times and no label from the other chat or bot.

## Blind review of the ffmpeg layer (Ruling 12)

The blind review of Task 2 measured memory blowups on the production stack (Alpine 3.24, ffmpeg 8.1.3). Small hostile files push one run to 1–6 GB:

| Hostile file | Size | Peak memory |
|---|---|---|
| zlib-compressed moov | 1 MB | 1.1 GB |
| huge sample index | 10 KB | 4.9 GB |
| 4:4:4 10-bit 4K | 2.7 MB | 1.6 GB |
| libvpx with a late oversized keyframe | 160 KB | 6.0 GB |

The last one bypasses `-max_pixels`, because the libvpx wrapper checks the frame size only after decoding. With four slots that is enough to OOM the host. These items are binding for Task 2. Where they differ from earlier items, they win.

1. **`-max_alloc 134217728` on both tools.** This is moved from "Later". The reviewer tested it: the cmov and stsz files are refused at about 45 MB, and 4K files still work.
2. **A per-child memory cap on Linux.**
   - Every tool runs through `/bin/sh -c 'ulimit -v <KiB> && echo 1000 > /proc/self/oom_score_adj && exec "$0" "$@"' <abs tool path> <args…>`.
   - The constant `toolMemoryLimit` is 1536 MiB. Verify on Alpine 8.1.3 that normal 4K H.264, 4K HEVC 10-bit, 1080p VP9 and the alpha sticker fixtures still render. Raise the constant, and say so, if one fails.
   - `exec` keeps the PID, so `Pdeathsig` and the kill still reach ffmpeg.
   - If `/bin/sh` is missing, video watching is off (fail closed) and the reason is logged.
   - On other OSes the cap does not exist. Document that.
3. **libvpx (the alpha path) only for stickers.**
   - `HasAlpha` is set only when the kind is `sticker`. Every other kind decodes VP8/VP9 with the native decoders, which honour `-max_pixels`.
   - A sticker must probe at ≤ 512 × 512 and ≤ 10 s, or it is refused. Mid-stream size changes are bounded by item 2.
   - Drop `libaom-av1` from the render list, so AV1 decodes only through `libdav1d`, which receives `-max_pixels`. If `libdav1d` is missing, AV1 is unsupported.
4. **Try `-fflags +noparse+nofillin`** on both tools, so codec parsers stop running on refused streams. Keep it only if the full suite passes on Windows 9.0.2, Alpine 8.1.3 and Ubuntu 6.1.1, including the colour/frame-time tests. Otherwise drop it and say so.
5. **Kill the whole process group on Linux.** Set `Setpgid`, and make `cmd.Cancel` kill the process group, so a wrapper script on PATH can't leave its child running. The Windows dev shim can still orphan ffmpeg; document that limit.
6. **Version log.**
   - When the gate turns video on, run `ffmpeg -hide_banner -version` once, through the same runner with a 5 s timeout, and log the first line.
   - Warn when the major version is below 8, or when it can't be read.
   - It is never fatal.
7. **Carried to Tasks 5 and 7:**
   - Each chat holds at most one video job, probe or render, at a time. A second one waits for the first.
   - Callers pass frame counts and durations already bounded by the bot's `video` config. `checkSheet`'s global bounds are only a backstop.
8. **Compose `mem_limit`.** The README (Task 8) recommends at least `slots × toolMemoryLimit + 512 MiB`. Whether to set it stays the operator's decision (Ruling 7).
9. **Not adopted yet: a sandbox for the tools.**
   - The reviewer rates the blast radius as Important. A future memory-corruption bug would run as the bot's user and could read every bot's config. No reachable code-execution CVE is known for 8.1.3 or 9.0.2.
   - A Landlock re-exec helper is the candidate fix: read-only access to the input directory, font and libraries; no writes; no TCP on ABI v4 or later. It is pending the user's decision.
10. **Declined:**
    - A pix_fmt allowlist. The memory cap bounds it, and a list would refuse legitimate 10-bit and screen-capture content.
    - A lower pixel cap. 4K phone video is common.
    - A native-decoder pre-pass before libvpx. Items 2 and 3 cover it.
    - A Windows Job Object. Windows is dev-only.

## Sandbox for the tools (Ruling 13)

The user chose, on 2026-11-16, to sandbox ffmpeg and ffprobe. This settles Ruling 12 item 9 and is built as **Task 2b** of the plan. That task's design section is binding. In short:

- **A re-exec helper runs every tool.** It is the bot's own binary, called as `__media_sandbox <spec> -- <tool> <args…>`. On one locked OS thread it applies, in order:
  1. rlimits: address space = `toolMemoryLimit`, core 0, fsize 0, nofile 64;
  2. `oom_score_adj` 1000;
  3. `no_new_privs`;
  4. Landlock at the best available ABI:
     - read/execute only beneath the system library and binary directories and `/etc`;
     - read-only on the input directory and the font;
     - no writes anywhere;
     - TCP bind/connect denied on ABI ≥ 4;
     - abstract unix sockets and signals scoped on ABI ≥ 6;
  5. finally an exec of the tool.
- **This replaces the `/bin/sh` `ulimit` wrapper from Ruling 12 item 2.**
- **Fail closed by default.** If Landlock is unavailable (any non-Linux OS, or an unsupported or seccomp-blocked kernel), video watching is off. `video.allow_unsandboxed: true` opts out, with a loud boot warning; Windows development uses it.
- **A gate self-test** checks that a read inside the input directory works, while a read outside it and any file creation both fail. It logs the result and the ABI.
- **Still not covered:** UDP and other non-TCP sockets on ABI < 6 or so. Ptrace of the bot is blocked by Landlock's domain rules, plus `PR_SET_DUMPABLE` from Task 8.

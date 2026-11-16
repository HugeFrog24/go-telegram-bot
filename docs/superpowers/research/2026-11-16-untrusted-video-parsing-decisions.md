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

## Blind review of the sandbox: seccomp and process containment (Ruling 16)

The blind review of the memory caps and Task 2b confirmed that file and secret isolation holds:
- `config/`, `bot.db`, other chats' videos and `/proc/<bot>` are all denied.
- Truncation is denied, and so is creating any file.
- Only fds 0–2 leak into the tool.

It also showed that on common kernels (Landlock ABI 4–7) a compromised tool can still:
- signal or kill the bot and other same-uid processes (below ABI 6);
- connect to same-uid pathname unix sockets (below ABI 9). Run natively, that includes the user's systemd/D-Bus bus, which is an escape.
- send UDP;
- open netlink sockets;
- fork or `setsid` children that outlive the run and escape the group kill, `Pdeathsig` and the per-process caps.

These items are binding for a Task 2b fix round.

1. **A seccomp-BPF filter in the helper.**
   - Install it after `no_new_privs` and Landlock, and before the exec, on the same locked thread. Build it with `x/sys/unix` only; no libseccomp.
   - Check the architecture first. Support `AUDIT_ARCH_X86_64` (rejecting the x32 bit) and `AUDIT_ARCH_AARCH64`. Any other architecture gets `SECCOMP_RET_KILL_PROCESS`.
   - Deny with EPERM, unless noted otherwise:
     - all socket syscalls: `socket`, `socketpair`, `connect`, `bind`, `listen`, `accept`/`accept4`, `sendto`, `sendmsg`, `sendmmsg`;
     - signals to anything but itself: `kill`, `tkill`, `tgkill`, `rt_sigqueueinfo`, `rt_tgsigqueueinfo`, `pidfd_open`, `pidfd_send_signal`. They are allowed only when the target is the tool's own PID, which the helper knows because it execs in place. ffmpeg's own `raise`/`abort` and thread signals must keep working;
     - process creation: `fork`, `vfork`; `clone` without `CLONE_THREAD`, or with any namespace flag; `clone3` (ENOSYS, so libc falls back to `clone`); `setsid`, `setpgid`;
     - kernel attack surface:
       - `ptrace`, `process_vm_readv`, `process_vm_writev`;
       - `unshare`, `setns`;
       - `io_uring_setup`, `io_uring_enter`, `io_uring_register`;
       - `bpf`, `perf_event_open`;
       - `keyctl`, `add_key`, `request_key`;
       - `userfaultfd`;
       - the mount family;
       - the module and kexec calls.
   - If seccomp is unavailable, the sandbox counts as unavailable (fail closed, same as no Landlock).
   - With seccomp in place, the gaps line names only what neither layer covers.
2. **`RLIMIT_CPU`** for both tools, at 2× the call's timeout, rounded up. That covers ffprobe, which has no `-timelimit`.
3. **Kill the process group after every run,** not only on a timeout or the output bound. Wait for the leader with `waitid(…, WEXITED|WNOWAIT)`, then `kill(-pgid, SIGKILL)`, then reap. The leader stays a zombie until it is reaped, so its PID can't be reused in between. This is defence in depth behind item 1.
4. **Input directory guard** in `newSandboxSpec`.
   - Resolve the directory. Refuse it when it is, contains, or lies beneath a bot directory: the working directory, `config/`, `data/`, or the video store root once Task 3 adds it.
   - Require it to hold exactly one entry.
   - Passing the input as an `fd:` with no directory grant stays on "Later".
5. **libvpx only on the sticker alpha path.** The render `-codec_whitelist` includes `libvpx`/`libvpx-vp9` only when the plan forces that decoder. A build without native vp8/vp9 then can't decode those codecs, and that is acceptable.
6. **Narrow the filesystem grants.**
   - Execute only on the tool binary and its ELF interpreter, which `debug/elf` reads from `PT_INTERP`.
   - Read only beneath the library directories, the font and fontconfig directories, and the two loader files, as traced on Alpine and Ubuntu.
   - Drop the blanket read and execute on `/usr`.
   - Document that Landlock can't hide `stat` metadata, such as file sizes and mtimes.
7. **Task 8 additions:**
   - compose `init: true`, so stray zombies are reaped if the bot is PID 1;
   - `pids_limit` as already planned;
   - the systemd example's `TasksMax` as already planned.

## Seccomp allowlist (Ruling 18)

A blind re-check and a scoped re-review of the Ruling 16 round agreed that the deny-list filter still lets a compromised tool hurt same-uid processes. Reproduced with the filter on:
- `prlimit64` on the bot at any ABI. Setting `RLIMIT_CPU` to 1 gets the bot SIGKILLed; `RLIMIT_NOFILE` set to 3 leaves it silently broken.
- `fcntl` `F_SETOWN`/`F_SETSIG` signals below ABI 6.
- SysV shared memory left behind after the run, beyond `RLIMIT_AS` (2 GiB measured).
- Thread and PID exhaustion that crashes the bot.
- `chmod`/`chown`/`utimensat`/xattr on files the tool can't read. Mode 000 on `bot.db` causes a crash loop.
- `setpriority`/`sched_*`/`ioprio` on other processes.
- Clearing Pdeathsig.
- inotify on bot directories.

A deny-list keeps missing classes like these, and it automatically allows every syscall a new kernel adds. These items are binding, and they **replace Ruling 16 item 1's deny-list design**.

1. **An allowlist filter.**
   - The architecture check comes first, unchanged.
   - Only syscalls on an explicit per-architecture allowlist are allowed. Everything else returns `ENOSYS`, so libc falls back the way it does for an unknown syscall. Argument-checked refusals return `EPERM`.
   - **How the allowlist is built:** trace ffprobe and ffmpeg (`strace -f -c`) on Alpine (musl) and Ubuntu (glibc) across the whole fixture matrix:
     - every codec and container: H.264, HEVC 10-bit, VP8, VP9, VP9 alpha via libvpx, AV1 via dav1d, GIF, MP4/MOV, MKV/WebM;
     - probe and render;
     - font and no-font fallback;
     - `-version`;
     - error paths: refused formats, timeouts, the output bound.
   - Add the libc baseline both libcs need.
   - Document each group in code with its reason.
   - **Argument checks on the allowed calls:**
     - `prlimit64`, `sched_getaffinity`, `sched_setaffinity`, `sched_getparam`, `sched_getscheduler`, and the `*_rt_sig*`/`kill`/`tkill`/`tgkill` family: allowed only for target 0 or the tool's own PID.
     - `clone`: only with `CLONE_THREAD` and no namespace flags.
     - `fcntl`: only an explicit set of commands, for example `F_GETFD`, `F_SETFD`, `F_GETFL`, `F_SETFL`, `F_DUPFD`, `F_DUPFD_CLOEXEC`, `F_GETLK`. `F_SETOWN`, `F_SETOWN_EX`, `F_SETSIG`, `F_SETLEASE` and `F_NOTIFY` are refused.
     - `prctl`: only the options the tools are seen to use, such as `PR_SET_NAME`/`PR_GET_NAME`. `PR_SET_PDEATHSIG` and anything else are refused.
     - `ioctl`: refuse `TIOCSTI` and `TIOCLINUX`, if `ioctl` is needed at all.
   - **Also allowed, because the helper needs them after the filter is installed:**
     - `prlimit64(0)`, `execve`, `write`, `exit_group`;
     - in check mode, the check's own probes. Those may fail with either `EPERM` or `ENOSYS`, and both count as denied.
   - **arm64** gets its own list, derived from the amd64 trace by name. It is tested with the BPF evaluator only, because no arm64 runtime is available. A wrong arm64 list fails closed through the gate self-test. Document that.
2. **`RLIMIT_NPROC`.** The helper sets it, soft and hard, to the uid's current task count plus `toolThreadHeadroom` (32), counted from `/proc` before Landlock. Verify that a 4K HEVC and an AV1 render stay within the headroom. Combined with compose `pids_limit` 256 (Task 8), four runs plus the bot fit.
3. **The self-check tests only the layers that are present.**
   - File checks run only when the Landlock ABI is above 0. Seccomp probes run only when seccomp is on. This restores `allow_unsandboxed` on hosts without Landlock, which the round broke, against Ruling 14 note 5.
   - New probes when seccomp is on:
     - `prlimit64` on the parent;
     - `F_SETOWN`;
     - `shmget`;
     - `chmod` on a file outside the input directory;
     - `inotify_init1`.
4. **The gaps line is accurate.**
   - It names what neither layer covers. With the allowlist, that is `stat` metadata reads plus whatever the trace forces.
   - It never claims "signals" or "metadata" are covered when they aren't.
5. **Check Alpine's libvpx 1.15.2-r1 for CVE-2026-1861**, using the Alpine secdb or the package changelog. Report the result. If it is unfixed, libvpx (the sticker alpha path) is refused until it is fixed.
6. **Nit:** the `sandboxSparesBot` comment names the current grants.

## Sandbox residuals after the allowlist (Ruling 19)

A scoped re-review and a blind re-check of the Ruling 18 round both found the allowlist correct and tight. The blind verdict was "no blocking issues". What remains is Minor, but two of the claims the sandbox logs are false, and a few cheap controls are missing. These items are binding for one more Task 2b fix round.

1. **Parent-death signal: accept the residual and correct the claims.**
   - A tool can still drop its parent-death signal. It starts a thread with `clone(CLONE_THREAD)` and calls `execve` of itself from that thread: `pdeath_signal` is per task and 0 for new threads, and `de_thread` keeps the PID. Both reviews reproduced this.
   - The resulting orphan keeps Landlock, the filter, the rlimits, its PID and its group. The group kill still reaches it while the bot lives. It outlives only a bot that dies, and only outside a supervisor: Docker's PID namespace and systemd's default `KillMode=control-group` both kill it.
   - Declined: a trusted per-run supervisor process. It would change the exec-in-place design of Ruling 14 for a Minor whose worst case is a sandboxed, sleeping process.
   - Fix the claims: the gap list entry "clearing the parent-death signal" (`sandbox.go`), the "no orphaned decoder" comment (`ffmpeg_linux.go`), and any similar comment. The gaps line names this residual and the supervisor kill as its backstop.
2. **The thread cap is a per-user pool; size it, and accept starvation by a compromised tool.**
   - `RLIMIT_NPROC` counts every task of the uid, so concurrent runs share their headroom. Legitimate 4-way concurrency measured a peak of 7 threads per tool, which leaves about 4 of 32 spare.
   - Measure the worst legitimate per-run peak on the production path, on both distros, for probe and render:
     - a self-made file with `toolMaxStreams` video streams;
     - 4K HEVC 10-bit;
     - AV1.
     The test must use the production arguments, with no `-threads` override.
   - `perRunBudget` = that peak + 4, and at least 12. If the peak is above 16, lower `toolMaxStreams` (not below 8) or give ffprobe `-threads 1` instead of raising the budget.
   - `toolThreadHeadroom` = 4 (the maximum `toolConcurrency`) × `perRunBudget` + 8 for the bot's own new threads.
   - Task 8 sets compose `pids_limit` and systemd `TasksMax` to 64 + 4 × `toolThreadHeadroom` + 64, rounded up to a multiple of 64, replacing the 256 planned in Ruling 12. A comment states the formula.
   - Residual accepted: a compromised tool can make other chats' concurrent renders fail for the rest of its run. Isolating runs needs a per-run cgroup, and Docker doesn't delegate one. The gaps line names it.
   - A tool's non-zero exit becomes a new `errToolFailed`, no longer `errUnsupportedMedia`, because it can be transient. The bot's own refusals (format, codec, streams, size, duration, kind) stay `errUnsupportedMedia`. Failure is still decided by exit status only; no stderr parsing.
   - Fix the comment that claims compose sets a `pids_limit`; compose doesn't yet.
3. **`RLIMIT_MEMLOCK`.** Set soft and hard to min(inherited hard, 64 KiB). Verify that the Ubuntu libgcrypt/gnutls path (a 16 KiB `mlock`) still works.
4. **Capabilities and root.**
   - The helper drops every capability before the task probe: `capset` with empty effective, permitted and inheritable sets, and `PR_CAP_AMBIENT_CLEAR_ALL`. `no_new_privs` then keeps `execve` from granting any back, root included. This closes raising hard limits through `prlimit64(0)` and `madvise(MADV_HWPOISON/MADV_SOFT_OFFLINE)`.
   - Order: capabilities → task probe → Landlock → filter → last limits → exec.
   - The check mode reports empty capability sets after the drop.
   - If the task cap is still not enforced after the drop (uid 0 in the initial user namespace is exempt), the sandbox counts as incomplete. Video is then off unless `video.allow_unsandboxed`. The log says to run the bot as a non-root user. Detect this empirically with the existing probe, not by comparing the uid with 0, so that root inside a user namespace still passes.
5. **Task probe termination.**
   - Clamp the bracket to the hard limit, guard against overflow, and cap the iterations. A probe that can't decide fails closed, as an error.
   - Red first: a unit test with a fake fork that fails above k, under a hard limit of `RLIM_INFINITY`.
6. **Futex: refuse the PI operations**, as Chromium's `RestrictFutex` does.
   - Allowed after masking `FUTEX_PRIVATE_FLAG` and `FUTEX_CLOCK_REALTIME`: `FUTEX_WAIT`, `WAKE`, `REQUEUE`, `CMP_REQUEUE`, `WAKE_OP`, `WAIT_BITSET`, `WAKE_BITSET`.
   - Everything else gets EPERM, including `LOCK_PI`, `LOCK_PI2`, `UNLOCK_PI`, `TRYLOCK_PI`, `WAIT_REQUEUE_PI` and `CMP_REQUEUE_PI`.
   - Re-run the trace matrix on both distros, plus the Go runtime in check mode.
7. **`-timelimit`.**
   - Today ffmpeg's `setrlimit(RLIMIT_CPU, {lim, lim+1})` fails with EPERM on every run, because the helper already set the hard limit to `lim`.
   - On Linux the helper's `RLIMIT_CPU` replaces `-timelimit`; keep `-timelimit` where no helper runs.
   - Test that render stderr has no "setrlimit".
8. **Logs are accurate in every mode that runs video.**
   - Full sandbox: the gaps line names everything uncovered:
     - stat metadata, including sizes, mtimes, path existence and process existence through `/proc/<pid>`;
     - `readlink` targets and `statfs`;
     - item 1's residual;
     - item 2's shared thread pool.
   - Under `allow_unsandboxed`, a gaps line for whatever layers are present.
     - Without Landlock it says the tool can read, write and run any file the bot's user can, including `/proc/<bot>/environ` (tokens) and `/proc/<bot>/mem` where Yama's `ptrace_scope` is 0.
     - Without the filter, it lists what only the filter closes.
   - The opt-out warning names reading, writing and running files, not just reading.
9. **Research, run by a separate agent in parallel with the fix round, without a code change.** List the ffmpeg CVEs fixed in 9.0 but not in 8.1.3, using ffmpeg.org/security.html and NVD; the blind re-check named CVE-2026-70628 (DVB subtitle parser) and CVE-2026-70632 (CFHD) as examples.
   - Judge each one's reachability through the per-kind `-format_whitelist`/`-codec_whitelist`, including the parsers `avformat_find_stream_info` initializes for every stream whatever the codec whitelist.
   - Where possible, confirm with harmless self-made files only.
   - The controller rules on any mitigation, such as refusing non-audio/video streams before the decode or requiring ffmpeg 9.0 in the image.

## ffmpeg version and probe-path exposure (Ruling 20)

A research pass checked whether ffmpeg vulnerabilities fixed in 9.0 but not in 8.1.3 are reachable through the bot's options. It used fix ancestry for about 70 CVEs, a subject comparison of n8.1-dev..n9.0.2 against n8.1..n8.1.3, source diffs, and gdb on Alpine 3.24 (8.1.3-r0) and edge (9.0.2-r3) with well-formed self-made files.

**Results:**
- **No reachable fix is missing from 8.1.3.** 8.1.3 was tagged on 2026-09-20, after 9.0.2, and it carries every reachable 9.0.x fix.
- **Scanners over-report.** CVE-2026-70628 (the dvbsub parser), 70629–70632 (rscc, screenpresso, tiff, cfhd), 66036, 66037, 96611 and others are listed as "before 9.0" or "through 8.1.2", but they are fixed in 8.1.3. Most are unreachable with the bot's options anyway.
- **The fixes that exist only in 9.0** are in code new in 9.0 or in components the bot doesn't reach: libdav1d T.35, LCEVC, matroska SMPTE 2094-50, the mov iTunSMPB discard, the swscale ops backend that 8.1.3 runs only with `SWS_UNSTABLE`, among others.
- **Unfixed in both versions:** CVE-2026-107678 (`pssh` recursion), which is a denial of service of the child only. It is reachable through mov in every kind except sticker. The probe and sheet timeouts, `RLIMIT_CPU` and the address-space cap contain it, and it surfaces as `errToolFailed`.
- **Probe sweep.** ffprobe runs every compiled-in demuxer's `read_probe` on the untrusted bytes, because the probe has no `-f`. Detection happens before the format whitelist is checked. The render already pins `-f`.
- **Parsers aren't covered by the codec whitelist.**
  - `-codec_whitelist` gates decoders only.
  - Matroska's `V_MS/VFW/FOURCC` and `A_MS/ACM` entries make nearly every audio and video parser in the build reachable.
  - The `extract_extradata` bitstream filter runs during stream-info gathering for streams without extradata.
  - Subtitle tracks are never parsed in mov or matroska.
- **ID3v2 is never parsed** for the whitelisted formats.

These items are binding.

1. **Stay on Alpine 3.24's ffmpeg 8.1.3.**
   - Task 8 pins the runtime stage to `alpine:3.24`, not `alpine:latest`, and not edge. A branch rollover must not silently change ffmpeg's major version.
   - Move to 3.25 or 8.1.4 deliberately. The monthly rebuild in the README picks up 3.24 point releases.
2. **The probe pins `-f`, chosen in Go from magic bytes.**
   - Read the file's first 12 bytes:
     - an atom type at offset 4 of `ftyp`, `moov`, `mdat`, `free`, `skip` or `wide` means `-f mov`;
     - `1A 45 DF A3` means `-f matroska`;
     - `GIF87a` or `GIF89a` means `-f gif`.
   - The chosen format must also be on the kind's format whitelist, which still applies.
   - No match means `errUnsupportedMedia`, and ffprobe never starts.
   - Tests:
     - every existing fixture still probes;
     - self-made wav, avi and mpegts files, and a file with a wrong magic for its kind, are refused without a tool run;
     - the probe's argv contains `-f`.
3. **Pin two invariants with tests:**
   - no decoder list contains the native `av1` decoder (libdav1d only);
   - no argument list contains `sws_flags` or `unstable`. 8.1.3 lacks the 9.0 ops-backend fixes.
4. **Declined, recorded as "Later":**
   - **A Go pre-scan of Matroska CodecIDs and MP4 sample entries.** It would be a second untrusted-input parser to maintain, and the sandbox already contains parser bugs.
   - **A custom minimal ffmpeg build.** It would shrink parsers and bitstream filters to the needed few, but we would then own its builds and security updates.
   - **Moving to 9.0 or edge.** It fixes nothing reachable and adds new reachable code. Several of those additions already needed post-release fixes.
5. **CVE-2026-107678 is accepted** as a contained denial of service. Watch 8.1.4 and 9.0.3 for the master fixes (71403a21b, 954606d99, 4a942afe8, 9fc8c785e).

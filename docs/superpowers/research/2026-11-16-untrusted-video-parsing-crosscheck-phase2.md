# Blind cross-check 0, Phase 2: my assessment compared with the research document

- **Compared:**
  - My independent Phase 1: `blind-crosscheck-0-phase1.md`, saved before this step and not edited since.
  - The other researcher's document: `docs/superpowers/research/2026-11-16-untrusted-video-parsing.md`, read in full (464 lines).
- **New local checks for this phase:** listed at the end. All used ffmpeg/ffprobe 9.0.2 (gyan.dev) on harmless samples I made myself.

**Overall:** the document's core design is sound and matches my independent result on the load-bearing points:
- a pre-parse `-format_whitelist` on every call;
- exact-name decoder allowlists, with a longer list for ffprobe;
- `-max_pixels`, a thread cap, bounded output and a semaphore;
- a minimal child environment;
- Alpine 3.24 / ffmpeg ≥ 8.1.3.

I found no Critical error in it. What it lacks is mostly on two fronts:
- **what a failure costs:** filtergraph injection, post-RCE reach, secrets in logs;
- **the LLM-tool side.**

There are also a few compatibility and factual gaps.

---

## Missed or under-weighted (ranked)

### Important

1. **Post-RCE blast radius is under-weighted, and native (systemd) deployments get no hardening at all.**
   - **The document's position:** it defers isolation (#16) and rlimits/`Pdeathsig` (#17) to "later". Compose hardening (#14) "needs approval" and is "the only hard memory bound available".
   - **What a same-uid ffmpeg can reach today:**
     - `/app/config/*.json` (Telegram and Anthropic/ElevenLabs keys);
     - `data/bot.db` (every chat's history; the document does not mention it);
     - all cached media;
     - `/proc/<bot-pid>/environ`;
     - the bot's memory via ptrace or `/proc/pid/mem` when Yama `ptrace_scope=0`;
     - full network access for exfiltration.
   - **Why that matters now:** about 133 CVEs are open "through 9.0.2" (§0.2 of my Phase 1). The allowlists shrink the surface; they don't make it bug-free.
   - **Native deployments:** `examples/systemd/telegram-bot.service` has no `NoNewPrivileges`, `ProtectSystem`, `PrivateDevices`, `MemoryMax` or `TasksMax`. A native deployment therefore has **no aggregate memory or pids bound**, and nothing stops a hijacked ffmpeg.
   - **Cheap "now" steps the document lacks:**
     - `prctl(PR_SET_DUMPABLE, 0)` in the bot at startup, so same-uid children can't read `/proc/<bot>/environ|mem` or ptrace it.
     - `Pdeathsig: SIGKILL`, a one-line Linux `SysProcAttr`.
     - The systemd directive set from my Phase 1 §2.5(e).
     - Compose hardening (#14) as the default rather than optional.
   - **Landlock is viable inside Docker:** the default seccomp profile allows the Landlock syscalls (moby commit af819bf, PR #43991), so a re-exec helper with Landlock plus a seccomp `socket` deny works there; bwrap does not. Worth stating so "later" has a concrete path.

2. **Filtergraph-injection impact is not analysed, and LLM tool arguments are not named as an injection source.**
   - **The document's T14:** it lists metadata and the font path, with the control "never put metadata or user text into a filtergraph". The control is right, but the stakes are not stated.
   - **What I verified on 9.0.2** (Phase 1 §4):
     - `drawtext=textfile=<path>` renders a local file into the frame (pixel-identical to the literal text).
     - The ≥ 7.1 option-from-file syntax `drawtext=/text=<path>` opens arbitrary paths.
     - `movie=`/`amovie=` sources **ignore the `-i` input's `-format_whitelist` and `-protocol_whitelist`**: `movie=evil.m3u8` and `movie=<ffconcat>` opened while the input had `-format_whitelist mov -protocol_whitelist file`.
   - **So any escaping slip becomes Critical:** file disclosure (config tokens) straight to the attacker through the LLM's description of the frame.
   - **Requirement:**
     - a constant filter template;
     - only Go-formatted, range-checked numbers (rejecting NaN, ±Inf and negatives);
     - never the model's raw strings (start, end and count are attacker-influenced via prompt injection);
     - a test that asserts the filter string matches `^[0-9a-z=:,/.()'\\ _-]+$` with no `movie`, `textfile` or `/`-prefixed option.
   - **Optional:** do labels, tiling and JPEG encoding in Go from fixed-size frames, which removes drawtext, freetype and tile from the attacker path.

3. **The LLM tool is not covered: cross-chat references, argument validation, budgets, and frames or metadata as prompt injection.**
   - **Background:** the brief states that tool arguments are attacker-influenced. The document covers only path naming (random refs, per-chat dirs, T2).
   - **Missing:**
     - Resolve video references only within (bot, chat, business connection), with opaque per-chat IDs. Never pass paths or Telegram file_ids to or from the model.
     - Validate `0 ≤ start < end ≤ probed duration`, finite values, and `1 ≤ frames ≤ N`.
     - Per-turn and per-chat budgets on ffmpeg runs and Files API uploads.
     - Never forward ffprobe tags (title, comment, handler_name) to the model unfiltered.
     - Treat text burned into frames as untrusted instructions.
   - This may live in the plan, but it is absent from this research.

4. **Bot token in logs through the download path, which the video feature reuses.**
   - **The flow:**
     1. `telegram_helpers.go:34-41` builds `https://api.telegram.org/file/bot<token>/<path>`.
     2. On any transport error, `http.DefaultClient.Do` returns a `*url.Error` whose `Error()` embeds that URL.
     3. It is wrapped with `%w` and logged by callers (`handlers.go:210`).
   - **Also:** no client timeout of its own, and `io.ReadAll` without a cap.
   - **Fix:**
     - redact (`errors.As(err, &urlErr)` → log only `urlErr.Err`);
     - a dedicated client with timeouts;
     - `io.LimitReader(resp.Body, 20<<20+1)`;
     - never pass the URL to ffmpeg.
   - Not attacker-triggered, but it leaks a full-takeover credential into journald or Docker logs.

### Minor

5. **The "unverified in 8.1.3" items can be resolved.**
   - The document marks CVE-2026-96611, -70632 and -30997 (and others listed only for 9.0) as unverified in Alpine 3.24.
   - **Source diffs show 8.1.3 has the fixes:**
     - `rscc.c`, `rasc.c`, `dvbsub_parser.c` and `cfhd.c` changed between n8.1.2 and n8.1.3, and are identical between n8.1.3 and n9.0.2.
     - The `lcldec.c` short-inflate fix (CVE-2026-66038) is in n8.1.3.
     - The `mov_read_ispe` `> INT_MAX` check (CVE-2026-96611) is in n8.1.3.
   - So CVE-2026-70628–70632, 12706/58049, 66038 and 96611 are fixed in 8.1.3. NVD/Alpine CPE ranges ("< 9.0") over-report, and scanners will flag 8.1.3 falsely.
   - CVE-2026-30997 (native av1) I did not diff; it is moot because native `av1` is off the lists.

6. **Some cheap caps are missing from the final argv:**
   - `-max_alloc` (the document tested 268435456 [L11] but did not adopt it);
   - `-max_streams 16` (the default is 1000; ffprobe opens a decoder per allowed stream);
   - explicit `-probesize 5000000 -analyzeduration 5000000`.
   - Cheap containment of allocation and stream-count amplification within 20 MB.

7. **The unpatched FLAC-parser recursion is missing from §2.3** (CVE-2026-125221, uncontrolled recursion in `score_header()`). FLAC-in-MKV/WebM reaches it during probing:
   - `matroskadec` sets `need_parsing` for every non-AAC audio track (n8.1.3 lines 2863–2868);
   - `flac` is on the probe list anyway.
   - DoS class, same treatment as the Vorbis parser.

8. **No second layer behind the format allowlist.**
   - **Verified:** DASH (`<MPD … dash:profile`) and ffconcat are detected **by content regardless of extension**. DASH opened `../file.mp4` and absolute `file:/…/file.mp4`; ffconcat opened same-directory names with `safe=1`.
   - The document's controls do block these (F (pre)). But its T2 is HLS-centric, and #8 sets `cmd.Dir` to the **per-chat video directory**, where other videos sit.
   - **Suggestion:** probe and render from a per-job directory (or hardlink) holding only the one input, so a regression in the allowlist doesn't immediately become a cross-file read.
   - Also narrow per type: sticker → `matroska` + `vp9`/`libvpx-vp9` only; video note and animation → `mov` + `h264`.

9. **The exfiltration channel is not framed.**
   - Frames reach the attacker through the model's description, so every uninitialised-memory decoder CVE is a confidentiality issue here.
   - The allowlist removes almost all of them. Two residual hygiene points:
     - the minimal `cmd.Env` also keeps secrets out of ffmpeg's own stack, which is exposed by stack-disclosure CVEs (125095, 125190, 125266, 120243, 120246);
     - don't echo raw sheets back into the chat by default.

---

## Disputed (ranked)

### Minor

1. **#5's probe-side cap rejects normal camera and screen media.**
   - The rule "reject if width or height > 4096, or width × height > 8,294,400" rejects:
     - DCI 4K 4096×2160 (8.85 MP);
     - GoPro 4K 4:3 4000×3000 (12 MP);
     - GoPro 5.3K 5312×2988 (15.9 MP);
     - DJI 5.4K;
     - 5K screen recordings 5120×2880 (14.7 MP).
   - All of these pass the document's own decoder cap `-max_pixels 16777216`, and short clips of them fit in 20 MB.
   - The cited priors (Mastodon, pict-rs) are hosting limits, not decode-safety limits.
   - **Suggestion:** align the probe cap with `max_pixels` (≤ 16.7 MP, sides ≤ 8192) and let `scale` downsize. (UX and compatibility, not safety.)

2. **§6.1's claim about cover art is incomplete.**
   - The claim: "the two mjpegdec records open in 9.0.2 are reached only through hwaccel".
   - **Verified locally:** with `-codec_whitelist mjpeg` only, the `mjpeg` decoder fully decodes a **JPEG-LS** bitstream (64×64 rgb24 frame output). `mjpegdec.c` (n9.0.2) dispatches to `ff_jpegls_decode_picture` / `ff_jpegls_decode_lse`.
   - So putting `mjpeg` on the probe list also brings in the JPEG-LS decoder: **CVE-2026-125243** (uninitialised memory into pixels, open through 9.0.2), alongside EXIF CVE-2026-125180.
   - **Impact is low:** probe-only, and ffprobe discards pixels. But the reasoning "decoder name = surface" is imprecise.
   - Optionally drop `mjpeg,png` from the probe list and accept that cover-art files fail, or keep them knowingly.

3. **The rationale for declining `-timelimit` (#20) doesn't hold, and `Pdeathsig` is deferred.**
   - "CPU time accrues faster than wall time with threads" only means the limit is set to `threads × wall budget`.
   - The real backstop question is whether ffmpeg outlives a dead bot:
     - Docker container teardown and systemd's default `KillMode=control-group` kill orphans;
     - plain native runs, `KillMode=process` or a crashed supervisor do not;
     - and `Pdeathsig` is "later".
   - Both `-timelimit` and `Pdeathsig` are one-liners on Linux. I'd adopt both now.

(I found nothing Critical or Important to dispute. The argv lines, the option order, the probe-list mechanics and the version floor are correct.)

---

## Confirmed: 18 load-bearing points (briefly)

1. `-protocol_whitelist file` does not stop local reads through demuxers (HLS `.m3u8` opened `file:` segments; DASH and ffconcat too).
2. `-format_whitelist mov,matroska,gif` must be **pre-parse on every call**. A post-hoc `format_name` check is too late. It rejects hls, dash, concat and xbin; aliases (`mp4`, `webm`) match.
3. Exotic decoders inside allowed containers need `-codec_whitelist`. It matches **implementation names** (`libdav1d` vs `libaom-av1`, `mp3float`, `srt`/`ssa`); I independently hit `av1` and `mp3`.
4. **ffprobe exits 1 if any stream's decoder is refused**, so the probe list must be longer. The document's §6.1 inventory (PCM, ADPCM-QT, text subtitles, covers) is more complete than mine and avoids breaking MKV+SRT and camera PCM MOVs. I adopt it.
5. `-map 0:V:0` (capital V excludes attached pictures) is better than my `0:v:0`.
6. ffprobe has no `-nostdin` (rc=1, "Option not found").
7. Input options must precede `-i`. After `-i`, `-protocol_whitelist file` blocks `pipe:1`.
8. `-max_pixels 16777216` on both tools; it catches mid-stream resolution changes.
9. `-threads 2` and `-filter_threads 1`.
10. `-frames:v 1`, `image2pipe` + `mjpeg`, `LimitReader`, stderr tail, `WaitDelay`, non-zero exit treated as failure.
11. A process-wide semaphore bounded by `ctx`.
12. Minimal `cmd.Env` (nil inherits the parent environment).
13. Bot-chosen names, never Telegram's extension. The `.avs`/AviSynth risk is real on the gyan build; Alpine lacks avisynth.
14. Never `-hwaccel`, `enable_drefs`, `use_absolute_path` or `-safe 0` (`enable_drefs` default false, verified).
15. Alpine 3.24 ships 8.1.3 and edge ships 9.0.2. Use `FROM alpine:3.24` instead of `:latest`, require ffmpeg ≥ 8.1.3, and note `community` gets fixes only until the next stable release.
16. The Nov-2026 "through 9.0.2" batch is open. On the allowlisted path the reachable items are DoS-class: pssh, HEVC PPS, AFGS1, VP9 leak, Vorbis parser, swscale/vf_scale.
17. LCEVC and hwaccel CVEs are not reachable (no liblcevc; no hwaccel).
18. The declines are right: stdin input (moov-at-end breaks it), `-count_frames`, re-encode sanitising, `-xerror`. A custom minimal build is the strongest long-term option.

---

## Phase 2 local checks

| Check | Result |
|---|---|
| `ffprobe -nostdin …` | rc=1, "Failed to set value '-v' for option 'nostdin': Option not found" (confirms the document's L9) |
| `ffmpeg -i x.mp4 -protocol_whitelist file … pipe:1` (option after `-i`) | "Protocol 'pipe' not on whitelist 'file'!" (confirms L8) |
| JPEG-LS image (`-c:v jpegls`) decoded with `-codec_whitelist mjpeg -c:v mjpeg` | rc=0; `framecrc` shows a decoded 64×64 rgb24 frame, so the mjpeg decoder runs JPEG-LS code |
| `mjpegdec.c` n9.0.2 | `CONFIG_JPEGLS_DECODER` paths call `ff_jpegls_decode_picture` / `ff_jpegls_decode_lse` |
| `isom_tags.c` n9.0.2 plus `ffmpeg -codecs` | `apac` maps to `apple_apac`, which has **no decoder**. iPhone spatial-audio tracks are skipped like `tmcd`, not a probe failure (supports the document's §6.1) |
| Pixel-cap arithmetic for common camera and screen sizes | 4096×2160, 4000×3000, 5312×2988 and 5120×2880 are rejected by the #5 probe cap but pass `-max_pixels 16777216` |

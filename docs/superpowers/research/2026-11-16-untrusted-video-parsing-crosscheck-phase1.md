# Blind cross-check 0, Phase 1: my own assessment, written before I read the other document

**Date:** 2026-11-16. **Scope:** a Go bot downloads untrusted Telegram media (≤ 20 MB) and runs ffprobe/ffmpeg through `os/exec` to read metadata and render JPEG frame sheets (filters fps, scale, drawtext, tile, overlay). It runs in Docker (Alpine package, non-root) or natively under systemd.

**What I read:** only the background section of `blind-security-review-prompt.md`, the repo's `Dockerfile`, `docker-compose.yml`, `examples/systemd/telegram-bot.service`, `config/default.json`, `telegram_helpers.go` and a few lines of `handlers.go`, plus public sources.

**Disclosure:** one repo-wide grep for `downloadTelegramFile` printed 5 lines of `docs/superpowers/plans/2026-11-16-video-watching.md`. Those lines say: download to a temp file, `probeMedia`, re-check the duration, `saveVideo`, `renderSheet`. I read nothing else there, and nothing under `docs/superpowers/research/`.

**Method:**
- CVE records came from the CVE.org API (`cveawg.mitre.org/api/cve/<id>`); the NVD API returned 503 throughout.
- Release and package data came from `ffmpeg.org/security.html`, `ffmpeg.org/releases/`, the Alpine APKINDEX/APKBUILD and security tracker, and the Debian security tracker.
- I diffed FFmpeg source between the n8.1.2, n8.1.3 and n9.0.2 tags.
- I ran local checks with ffmpeg/ffprobe 9.0.2 (gyan.dev build) on harmless samples I made myself (lavfi test patterns, hand-written playlists, a canary text file). Section 4 lists every check.

---

## 0. Facts that change the picture

1. **The Docker image does not ship ffmpeg 9.x.**
   - The `Dockerfile` uses `FROM alpine:latest`, which today is Alpine **3.24.4** (released 2026-11-05).
   - Alpine 3.24's `ffmpeg` is **8.1.3-r0** (community index, dated 24-Oct-2026). Only Alpine **edge** has `ffmpeg-9.0.2-r3`.
   - So "ffmpeg 9.x in Docker" is true only if the image moves to edge or to the next stable release.
   - `alpine:latest` will silently jump to 3.25 later, along with its ffmpeg major version. That is a reproducibility and compatibility hazard: options and decoder names change.
2. **Every FFmpeg release is affected by a large unpatched batch.**
   - The newest releases are 9.0.2 (2026-09-18) and 8.1.3 (2026-09-21).
   - On 2026-11-09, about 100 CVEs were published, most titled "FFmpeg through 9.0.2" (CVE-2026-125064…125270). A batch on 2026-10-08 (CVE-2026-1076xx) and one on 2026-11-01 (CVE-2026-1202xx) add more.
   - No release fixes them yet; the Debian tracker lists them as "vulnerable".
   - **Version pinning alone cannot make this safe today.** Shrinking the attack surface and sandboxing are the real controls.
3. **8.1.3 already has most 9.0 fixes.**
   - The NVD/Alpine CPE ranges say CVE-2026-70628/70629/70630/70631/70632, CVE-2026-12706 and CVE-2026-96611 affect "< 9.0".
   - The source says otherwise:
     - `rscc.c`, `rasc.c`, `dvbsub_parser.c` and `cfhd.c` changed between n8.1.2 and n8.1.3, and are byte-identical between n8.1.3 and n9.0.2.
     - The `lcldec.c` zlib short-inflate fix (CVE-2026-66038) is in n8.1.3.
     - The `mov_read_ispe` `> INT_MAX` check (CVE-2026-96611) is in n8.1.3.
   - So for known fixes, 8.1.3 and 9.0.2 are roughly equivalent. CPE-based scanners will over-report 8.1.3.
4. **The frames go to an LLM, and its reply goes back to the attacker.** Any local-file read, or any uninitialised-memory disclosure that ends up in decoded pixels, becomes **data the attacker can see**. The model can be asked to transcribe or describe the frame. So the many 2026 "uninitialized memory disclosure" decoder CVEs and every "local file read into frames" technique are **confidentiality** issues here, not just DoS.
5. **The local test build is not representative.**
   - gyan.dev 9.0.2 "essentials" is built with `--enable-avisynth`, libgme, libopenmpt, libssh, libsrt, libzmq, etc. It has 374 demuxers, 551 decoders and about 38 input protocols.
   - Alpine's build differs: libdav1d, libjxl, libopenmpt, libbluray, libdvdread/dvdnav, libxml2 (the DASH demuxer), librist, libsrt, libssh, libzmq, vulkan/vaapi/vdpau. It has **no** avisynth and no liblcevc_dec.
   - Results such as which decoder name handles AV1 differ between builds (see 2.4).

---

## 1. CVEs from 2022 to today: what this pipeline can reach

"Default" means an ffmpeg/ffprobe call with no demuxer, decoder or protocol allowlist (plain `ffprobe file`). "With allowlist" means my requirements R1 and R2 (§3): `-format_whitelist mov,matroska,gif -protocol_whitelist file`, plus a `-codec_whitelist` limited to the expected decoders. Probing alone (`avformat_find_stream_info`) opens decoders and parsers for **every** stream. JFrog demonstrated RCE via ffprobe metadata extraction for CVE-2026-8461. So "only ffprobe" is not a safe subset.

### 1.1 Unpatched in every release today, and reachable

| CVE | Component | Impact | CVSS 3.1 | Fixed in | Reachable? |
|---|---|---|---|---|---|
| CVE-2026-125183 | HEVC `pps_3d_extension` (`hevc/ps.c`) | CPU exhaustion: hundreds of millions of bit skips. "Stalling decoders, parsers, and ffprobe" | 5.5 | git (pr 25572), no release | **Yes, even with an allowlist** (HEVC is needed for iPhone video). Mitigate with wall-clock and CPU limits |
| CVE-2026-125185 | AFGS1 film-grain parser (T.35 SEI in HEVC/VVC) | Division by zero, crash | 5.5 | git, no release | **Yes** (HEVC). Crash of the child only |
| CVE-2026-125223 | VP9 decoder (8.0–9.0.2) | Memory leak per bad tile with slice threading | 4.7 | git, no release | **Yes** (VP9 stickers). Mitigate with memory limits and `-threads` |
| CVE-2026-107678 | MOV `mov_read_pssh` → recursive `av_encryption_init_info_free` | Stack exhaustion plus quadratic CPU from tens of thousands of `pssh` boxes | 4.7 | git (pr 24593), no release | **Yes** (mov demuxer). DoS only |
| CVE-2026-125180 | `libavcodec/exif.c` (8.1–9.0.2) | Quadratic memory from overlapping IFDs in JPEG/PNG/TIFF/WebP | 5.5 | git, no release | Only if mjpeg/png decoders are allowed (cover art). Memory DoS |
| CVE-2026-38343 … 38350 | `vf_scale.c`, libswscale (`slice.c`, `utils.c`, `output.c`, `alphablend.c`) | Integer overflows, NULL dereference and division by zero (DoS); 38347 is a heap overflow in `ff_sws_alphablendaway` | 6.5–7.5 | Not stated; Debian marks "vulnerable" | **`scale` is in our filtergraph**: probably reachable as DoS with crafted dimensions/pix_fmts. 38347 needs alpha "blend-away" (yuva → non-alpha conversion with alpha blending; not the default as far as I know, verify). Mitigate by bounding dimensions in Go before decode, plus `-max_pixels` |
| CVE-2026-125221 / 125224 | FLAC parser (recursion) / Vorbis parser (CPU) | DoS | 5.5 | git | **Yes, via WebM/MKV audio tracks.** Parsers run during probing and are **not** covered by `-codec_whitelist` or by `-an` (matroskadec sets `need_parsing` on non-AAC audio) |
| CVE-2026-125097 | `rawvideo` decoder (`rawdec.c`) | **Uninitialised heap disclosed into pixels** (AVI/**MOV**/NUT) | 3.3 | git | Yes by default via MOV. **No** with a decoder allowlist that excludes rawvideo |
| CVE-2026-125253 | `cbs_vp9.c` superframe | OOB read, 32-bit builds only | 5.3 | git | No on amd64/arm64 (the compose file builds those). Yes on armv7 |
| CVE-2026-120210 | IMF demuxer ASSETMAP | **Path traversal**: opens local MXF files and probes file existence/size | 4.3 | git | Default: if the imf demuxer is probed. No with the format allowlist |
| CVE-2026-107677 / 75146 | DASH demuxer | Infinite loop / OOB read | 4.7 / 8.1 | git | **Default: yes. DASH is detected by content regardless of extension (verified, §4).** No with the allowlist |
| CVE-2026-125226 | SBaGen demuxer | Large allocation | 5.5 | git | Default yes (content probe). No with the allowlist |
| CVE-2026-125262 / 125263 | LCEVC (`lcevcdec.c`) | Heap overflow 7.8 / memory | 7.8 / 4.7 | git | **Not reachable**: needs `--enable-liblcevc-dec`, which neither Alpine nor gyan uses |

Exotic decoders reachable **only without a decoder allowlist**: a MOV/MKV/AVI track can name any FourCC or CodecID, and probing opens the decoder. These are unfixed through 9.0.2.

Memory-corruption or OOB issues:
- FLIC (CVE-2026-125181, 125241)
- Huffyuv/FFVHUFF (125229 is the encoder; 120247 is a decoder disclosure)
- TDSC cursor (125178)
- Musepack SV7 (125171)
- APE memory (125104)
- Interplay ACM (125207)
- PSD (125213, 32-bit only)
- HAP (125222, 32-bit only)
- Blu-ray PCM (125214, big-endian only)
- Cook (125182)
- many NULL dereferences (125064–125073)

Uninitialised memory **into pixels**:
- targa (125170)
- FSV/FSV2 (125175/125176)
- MSCC/SRGC (125177)
- VQA (125179)
- CamStudio (125184)
- MVC2 stack (125190)
- DXA (125192)
- JPEG-LS (125243)
- SANM stack (125266)
- CFHD (125268)
- C93 stack (125095)
- Tiertex SEQ (125096)
- WebP ALPH (125098)
- GEM (125099)
- VMnc (125101)
- TAK (125102)
- ALS/MLZ (125103)
- G2M stack (120243)
- PCX (120244)
- Sun Raster (120245)
- CDXL stack (120246)
- Huffyuv (120247)

### 1.2 Fixed in both 8.1.3 and 9.0.2, but critical in an unrestricted pipeline (sets the version floor)

| CVE | Component | Impact | CVSS | Fixed | Reachable by default? |
|---|---|---|---|---|---|
| CVE-2026-8461 ("PixelSmash") | MagicYUV decoder | Heap OOB write. JFrog showed RCE via ffprobe metadata scans (Jellyfin) and via thumbnailers (ASLR off). Delivered in AVI/MKV/MOV | 8.8 | 8.0.3 / 8.1.2 / 9.0 | Yes before the fix, and the codec is exotic. Allowlist blocks it |
| CVE-2026-58049 / 12706 | RASC decoder | OOB write 8.6 / UAF 6.5 | 8.6 / 6.5 | 9.0; backported in 8.1.3 (source diff) | Allowlist blocks |
| CVE-2026-64830 | VobSub demuxer (`mpeg.c`) | Heap overflow | 8.8 | > 8.1.2 | Content-probed (`.idx` magic). Allowlist blocks |
| CVE-2026-64835 / 66039 / 65703 / 18393 / 125203 / 125267 / 125244 | ADX, MACE6 (CAF), TDSC, Cook, APAC, VVC `sh_entry_points` | Heap OOB writes | 7.8–8.8 | > 8.1.2 / 8.1.3 / 9.0.2 | Allowlist blocks (VVC too: exclude `vvc`) |
| CVE-2026-70628 | DVB subtitle **parser** via WTV | Signed overflow → heap overflow | 7.8 | 9.0; 8.1.3 (source diff) | Format allowlist blocks (parsers are not covered by codec_whitelist) |
| CVE-2026-70632 | CFHD, **during stream probing** | Heap OOB write | 7.8 | 9.0; 8.1.3 | Allowlist blocks |
| CVE-2026-66038 / 70629 / 70630 / 70631 | LCL/ZLIB, RSCC, Screenpresso, TIFF | Uninitialised heap → pixels | 5.5–6.5 | 9.0; 8.1.3 | Allowlist blocks |
| CVE-2026-40962 | **MOV demuxer** CENC subsample | Integer overflow → OOB write | 4.9 | 8.1 | **mov is allowed**, so the floor must be ≥ 8.1 |
| CVE-2026-96611 | **MOV demuxer** HEIF `ispe`/grid | Signed overflow → wrap | 6.9 | 9.0 (bounds check present in n8.1.3) | mov is allowed. Fixed in 8.1.3 |
| CVE-2026-30999 | `av_bprint_finalize` | Heap overflow | 7.5 | 8.0.3 / 8.1.2 / 9.0 | Generic |
| CVE-2026-30997 | Native AV1 decoder `read_global_param` | OOB read | 7.5 | 9.0 (8.1.3 not checked) | The native av1 decoder is not the default when libdav1d/libaom exist |
| CVE-2026-107697 / 107695 | HLS: child playlist **bypasses protocol and allowed_extensions** / self-reference loop | Policy bypass / DoS | 4.3 / 6.5 | 8.1.3 | HLS is detected only with an `.m3u8`/`.m3u` name or mime (verified). Allowlist blocks it entirely |
| CVE-2025-59728 / 59729 / 59730–59734 | DASH heap write, DHAV, SANM, EXR DWA | Heap writes / UAF | to 8.7 (v4) | 8.0 | Allowlist blocks |
| CVE-2025-9951 / 22921 | JPEG2000 `cdef` | Heap overflow ("RCE") / segfault | 7.2 (v4) / 6.5 | 8.0 | Allowlist blocks |
| CVE-2025-69693 | RV60 | OOB read | 5.4 | 8.1 | Allowlist blocks |
| CVE-2023-6601 / 6602 / 6604 / 6605 | HLS: data-URI extension bypass / **tty-demuxer exfiltration** / xbin / **DASH SSRF** | Policy bypass, file exfiltration into frames, SSRF | 4.7 / 5.3 / 5.3 / 7.2 | 7.1.1 / 8.0 | Shows the attack class directly. Allowlist blocks |
| CVE-2024-7055 / 7272 | PNM decoder / swresample | Heap overflows | 6.3 | 7.0.2 / 6.0 | — |
| CVE-2024-22860 / 22862 | JPEG XL anim demuxer / parser | Integer overflow → RCE | 9.8 | 6.1 | Allowlist blocks |
| CVE-2024-32228 / 36615 | HEVC `hevc_frame_end` SEGV / VP9 frame-thread race | DoS / info leak | 6.6 / 5.9 | 7.0.x / 7.1 | Core decoders, fixed |
| CVE-2022-2566 | **MOV** `build_open_gop_key_points` | Heap write | 9.0 | 5.1.1 | Fixed long ago, but shows the mov demuxer is RCE-class surface |
| CVE-2022-48434 | `pthread_frame` stale hwaccel → UAF | RCE (D3D11) | 8.1 | 5.1.2 | Only with hwaccel |

### 1.3 Not reachable in this pipeline (for completeness)

- **Encoders and muxers:**
  - GIF encoder 125250, libwebp encoder 125204
  - APNG/PNG encoders 125218/66040, DVB/DVD sub encoders 125233/125209
  - hvcC writer 75141, MPEG-PS mux 75142
  - RTP packetizers 30754/75144/75145/75147, spdif 64833
  - AAC encoder 2025-1594, rpza/smc encoders 2022-3964/3965
- **hwaccel paths** (Vulkan/VAAPI/VDPAU/DXVA/NVDEC/QSV): 64831, 64832, 125201, 125202, 125219, 125239, 125249, 125255, 125265
- **Network protocols:** RIST 75143 (CVSS 9.8, via `async:rist://`), RTSP redirect SSRF and loop 107698/107696, sftp host-key 107675, mbedTLS 107660
- **BSFs:** dts2pts, lcevc_metadata, av1_frame_merge, aac_adtstoasc, and cbs_sei via h264_metadata (125269)
- **Tools and filters we don't use:** zmqsend 30998, TF dnn 2025-12343, firequalizer 2025-10256, af_pan 2025-0518

This holds as long as we never stream-copy, never output those formats, never pass `-hwaccel`, and keep `-protocol_whitelist file`.

### 1.4 Libraries (Alpine 3.24 versions)

| Library | Version | Notable CVEs | Reachable? |
|---|---|---|---|
| libvpx | 1.15.2 | CVE-2023-5217 (VP8 **encoder** heap overflow, exploited in the wild, fixed 1.13.1); 2023-44488; 2024-5197 (img_alloc overflow, 1.14.1); 2025-5262 (encoder double free) | No. All fixed, and ffmpeg's native vp8/vp9 decoders win over libvpx (`allcodecs.c` order) |
| dav1d | 1.5.3 | CVE-2024-1580 (integer overflow, fixed 1.4.0) | AV1 path on Alpine (`libdav1d` is registered before `libaom-av1` and native `av1`). Fixed |
| libaom | 3.14.1 | CVE-2024-5171 (3.9.0), CVE-2023-6879 | Not the default AV1 decoder on Alpine. It **is** the AV1 decoder in the local gyan build (no dav1d) |
| freetype | 2.14.3 | CVE-2025-27363 (≤ 2.13.0 OOB write, exploited in the wild) | Fixed. Not attacker-reachable while drawtext uses a fixed `fontfile` (drawtext never loads MKV font attachments; only the `subtitles`/`ass` filters do) |
| fontconfig / harfbuzz / fribidi | 2.17.1 / 13.2.1 / 1.0.16 | none open | Avoid fontconfig entirely by passing `fontfile=` |
| **libxml2** | 2.13.9 | Several open per CPE (CVE-2025-6021, 2025-8732, 2026-0989, 2026-6732, 2026-11979, 2026-86137…86144); libxml2 is thinly maintained since 2025 | **Reachable by default** through the DASH demuxer on any file containing `<MPD … dash:profile` (verified). Allowlist blocks |
| libjxl | 0.11.2 | CVE-2024-11403/11498 flagged by CPE | jpegxl demuxers. Allowlist blocks |
| libopenmpt | 0.8.10 | — | Module demuxer is content-probed (large C++ parser). Allowlist blocks |
| libwebp | 1.6.0 | CVE-2023-4863 (fixed) | ffmpeg uses libwebp only to encode |
| librist / libsrt / libssh / zeromq / libbluray / libdvdread | — | RIST 2026-75143 (9.8) | Protocols. `-protocol_whitelist file` blocks them |

---

## 2. Attack techniques against "ffmpeg on user uploads", with mitigations

### 2.1 Container references, playlists and protocols (local file read, SSRF, cross-chat read)

- **HLS (`#EXTM3U`)**
  - Segments can be `file:` or `http(s):` URLs.
  - Verified on 9.0.2: a playlist named `.m3u8`/`.m3u` is detected by content and opens `file:/abs/path/seg.ts`.
  - With a bot-chosen name (`.mp4`, `.bin`, none), 9.0.2 refuses ("Not detecting m3u8/hls with non standard extension"). That same check is in n8.1.3 `hls.c:2808`.
  - Non-media and `data:` segments are refused by `allowed_segment_extensions` / `extension_picky` (verified). Media files are not.
  - Known exploits: neex 2017 (HackerOne #237381, WordPress: playlist hidden in AVI GAB2 subtitles, xbin/tty to render files); the Flickr HLS file read (H1 #1062888); CVE-2016-1897/1898; CVE-2023-6601/6602/6605; CVE-2026-107697; Rails Active Storage CVE-2026-126188 (published 2026-11-10: HLS opened attacker-named `file://` segments during preview).
- **DASH (`<MPD`)**
  - Detected **by content regardless of extension** (`dash_probe`, n8.1.3; verified on 9.0.2 with `.mp4` and no extension).
  - Opens `BaseURL`s:
    - relative (`normal.mp4`): verified
    - **`../normal.mp4`**: verified
    - **absolute `file:C:/…/normal.mp4`**: verified
  - It is limited only by `allowed_extensions="aac,m4a,m4s,m4v,mov,mp4,webm,ts"`.
  - `http:` is blocked by the local-input default whitelist `file,crypto,data` (verified).
  - It also puts libxml2 on the attack surface.
  - **Impact here:** an attacker sends an "mp4" that is really an MPD pointing at another chat's cached download (`data/…/<name>.mp4`). ffmpeg renders that video into the attacker's frame sheet, and the LLM describes it. That is cross-chat disclosure.
- **ffconcat (`ffconcat version 1.0`)**
  - Detected **by content regardless of extension** (`concat_probe`; verified with `.mp4` and no extension).
  - Opens sibling files by relative name.
  - `safe=1` blocks absolute paths and `..`, but **not same-directory names** (verified). So it reads any file in the same directory as the upload, which is the same cross-chat read when downloads share a directory.
- **Other content-probed text formats that render text into video:** xbin (verified detected from content), tty (extension-gated since the CVE-2023-6602 fix), bintext/adf/idf. These are the exfiltration renderers that combine with the playlist tricks above.
- **IMF** (CVE-2026-120210 path traversal); **VobSub `.idx`** (opens a companion `.sub`; CVE-2026-64830); **SDP/RTP/RTSP** (network; blocked by the default whitelist, but RTSP redirects bypassed `-protocol_whitelist` before 8.0.2, CVE-2026-107698).
- **MOV `dref`/alias external tracks:** off by default (`enable_drefs=false`, verified in `-h demuxer=mov`). Never set it.
- **AviSynth/VapourSynth by extension:**
  - `.avs` is evaluated as a script by builds with `--enable-avisynth`. Active Storage CVE-2026-126186 shows file disclosure and possible RCE.
  - **The local gyan build has avisynth.** ffmpeg.org's GPL Linux builds do too. Alpine doesn't.
  - The extension decides the demuxer when the content isn't recognised.
- **Protocol names inside the path** (`concat:`, `subfile:`, `data:`, `http:`, `async:`): only if a path or URL comes from outside. Never pass the Telegram download URL to ffmpeg: it contains the bot token, would show in `/proc/*/cmdline` and in errors, and would enable network protocols.
- **Filtergraph source filters** (`movie=`, `amovie=`): **verified to ignore the input's `-format_whitelist`/`-protocol_whitelist`**. `movie=evil.m3u8` and `movie=cc.mp4` (ffconcat) opened fine while the `-i` input had `-format_whitelist mov -protocol_whitelist file`. Any filtergraph injection is therefore a full bypass.

**Mitigations:**
1. Pass `-protocol_whitelist file -format_whitelist mov,matroska,gif` to **both** ffprobe and ffmpeg (verified to reject HLS, DASH, ffconcat and xbin, and to accept mp4/mov, webm/mkv and gif).
   - Nested contexts inherit the whitelists (`ff_copy_whiteblacklists` in hls.c, dashdec.c, concatdec.c and avidec.c GAB2, n8.1.3).
   - Per media type, narrow further: sticker → `matroska` only; video note and animation → `mov` only; "document" → `mov,matroska`, plus `gif` only for real GIFs.
2. On the second run, force the demuxer the probe found: `-f mov|matroska|gif`.
3. Before calling ffmpeg, sniff magic bytes in Go and reject anything else:
   - `ftyp` at offset 4 for ISO-BMFF
   - `1A 45 DF A3` for EBML
   - `GIF87a`/`GIF89a` for GIF
4. Bot-chosen file names: random, a neutral fixed extension (or none), absolute, passed as `file:/abs/path`. Never use Telegram's `file_name`, `mime_type` or `file_path` extension.
5. **One private directory per job, containing only that job's input.** Keep the media cache in a directory separate from `data/bot.db`, with per-bot/per-chat subdirectories and unguessable names. A playlist that slips through then has nothing useful to reference.
6. Never set `enable_drefs`, `-safe 0`, or `allowed_extensions=ALL`.

### 2.2 Exotic decoders, parsers and probe-time decoding (memory corruption, memory disclosure)

- Any FourCC or CodecID in MOV/MKV/AVI selects a decoder. Probing opens it (CVE-2026-8461 RCE via ffprobe; CVE-2026-70632 "during stream probing").
- **Mitigation: `-codec_whitelist` with exact decoder *implementation* names on both programs.** Verified behaviour:
  - Matching is on the implementation name, not the codec ID. `av1` does **not** allow `libaom-av1`, and `mp3` does **not** allow `mp3float`. The chosen default decoder must be listed: there is no fallback to a listed one.
  - The names are build-specific: AV1 is `libdav1d` on Alpine and `libaom-av1` locally. Compute or check them at startup (parse `ffmpeg -decoders`), or force `-c:v <name>` per probed codec.
  - **ffprobe exits 1 with empty JSON if an audio stream's decoder is not allowed** (verified with an AAC phone-style MP4, even with `-select_streams v`). The ffprobe list must include the usual audio decoders (`aac,mp3float,opus,vorbis,flac,ac3,eac3,alac,pcm_s16le,…`).
  - ffmpeg with `-an` tolerates a video-only list: it warns and returns 0.
- Suggested sets:
  - **Video:** `h264,hevc,vp8,vp9,libdav1d` (or `libaom-av1`), `gif`. Add `mpeg4,h263` for old 3GP. Add `mjpeg,png` only if cover art or MJPEG video matters; they bring EXIF CVE-2026-125180.
  - Exclude `rawvideo`, `vvc`, `prores` (optional) and everything else.
- **Parsers are not covered by codec_whitelist.** They run in the demuxer during probing: for example the FLAC/Vorbis parsers on MKV audio, and the DVB/DVD subtitle parsers in TS/WTV/VOB. The format allowlist is the main control. `-fflags +noparse+nofillin` is a possible extra, but must be tested for compatibility.
- `-hwaccel` is never passed. Under systemd, add `PrivateDevices=yes`.
- `-threads 1..2` limits frame-thread races and memory multiplication (CVE-2024-36615, CVE-2026-125223).
- Map only `-map 0:v:0` with `-an -sn -dn`.

### 2.3 Filtergraph and argument injection

- **Verified:**
  - `drawtext=textfile=canary.txt` renders a local file into the frame (pixel-identical to the literal text).
  - The ffmpeg ≥ 7.1 option-from-file syntax `drawtext=/text=<path>` opens arbitrary paths ("Error loading value for option 'text' from file …" for a missing file).
  - `movie=`/`amovie=` open any URL or demuxer, bypassing the input whitelists.
  - ffmpeg ≥ 7.0 also supports the CLI form `-/option <file>`.
- **Escaping is error-prone:** my first attempt at a literal `00:01` label broke the graph. drawtext needs two escaping levels (`text='00\:01'`).
- Injection sources:
  - LLM tool arguments (start, end, count)
  - Telegram `file_name`, `mime_type`, caption
  - ffprobe tags (title, handler_name)
  - config values
- **Mitigations:**
  1. A constant filter template where only Go-formatted numbers are substituted (`strconv.FormatFloat(x,'f',3,64)` / `Itoa` after a range check).
  2. No string from the user, the file, the model or metadata ever goes into argv option-name positions or into the filter string.
  3. drawtext gets `fontfile=<absolute bundled font>` and `expansion=none`, and its text is built from a strict charset (`[0-9:.]`) and escaped. Never use `%{metadata:…}`, `textfile=` or `/opt=`.
  4. Better: let ffmpeg only decode and scale to fixed-size frames (`-f rawvideo -pix_fmt rgb24 pipe:1`, or one JPEG per frame), then do labels, tiling and JPEG encoding in Go.
     - That removes drawtext, freetype, harfbuzz and fontconfig, plus tile and overlay, from the attacker path.
     - The output size becomes exactly `w×h×3×N` bytes.
  5. Input path is `file:<absolute>` (never starts with `-` and contains no protocol prefix). `exec.Command` with an absolute ffmpeg path found at startup; no shell.

### 2.4 Resource bombs (CPU, memory, disk, processes, timestamps)

- **Pixel bombs:** a tiny file declares 16k×16k. H.264/HEVC SPS and VP9 frames can **change resolution mid-stream**, after the probe-time check.
  - **Mitigation:** `-max_pixels 16777216` on both programs. Verified: decoding fails for 2000×2000 with `-max_pixels 1000000`, and ffprobe fails too. Also check w/h in Go after the probe.
  - Keep `reinit_filter` at its default (on). CVE-2026-65705/66036/66041 show filters overflowing when `-reinit_filter 0` meets a size change.
- **Decode CPU bombs:** CVE-2026-125183 HEVC PPS, Vorbis/FLAC parsers, the HLS/DASH loops, and plain hard-to-decode streams.
  - **Mitigation:** `exec.CommandContext` with a deadline (ffprobe about 10 s, ffmpeg about 30–60 s), `cmd.WaitDelay`, process-group kill (`Setpgid` plus kill `-pgid`) and `Pdeathsig=SIGKILL`.
  - Add `-timelimit <cpu-seconds>` (RLIMIT_CPU; "not implemented" on Windows).
- **Memory:** leak/alloc CVEs (125223, 125180, 66037 IAMF, 125207, 125104), giant sample tables in MOV, frame-thread pools.
  - **Mitigation:** `-max_alloc 536870912` (global), `-threads 2`, `-filter_threads 1`, `-max_streams 16`, `-probesize 5000000`, `-analyzeduration 5000000`.
  - Plus an OS limit: cgroup (`mem_limit`/`MemoryMax=`), and/or `prlimit` on the child (RLIMIT_AS around 1–2 GB, RLIMIT_FSIZE, RLIMIT_NOFILE, RLIMIT_CORE=0).
- **Duration and timestamp bombs:** a huge claimed duration or a PTS gap lets the `fps` filter duplicate frames millions of times.
  - **Mitigation:** clamp `-ss`/`-t` to a validated window inside the probed duration, cap the probed duration (reject > N minutes), always `-frames:v 1` per sheet, and use timeouts.
- **Output and disk:**
  - Write to `pipe:1` and read with `io.LimitReader`. If a file is used: `-fs`, `-update 1`, a fresh per-job dir, `-n`.
  - Cap stderr capture with a limited writer and `-loglevel error -nostats -hide_banner`.
  - `-nostdin` (ffmpeg's interactive keys include `c` for sending filter commands).
- **Concurrency:** a global semaphore (for example NumCPU/2), at most one job per chat, a bounded queue, and reject-when-busy. In the container: `pids_limit`, `cpus`, `mem_limit`.
- **Download:** `io.LimitReader(resp.Body, 20<<20+1)`, reject if over. The current `downloadTelegramFile` uses `http.DefaultClient` and unbounded `io.ReadAll`.

### 2.5 What a compromised ffmpeg can reach (post-RCE blast radius)

- **Today:** ffmpeg runs as the same uid as the bot, so it can read:
  - `/app/config/*.json` (Telegram tokens, Anthropic/ElevenLabs keys; the mount is `:ro` but readable)
  - `data/bot.db` (all chat history)
  - every cached download
  - `/proc/<bot-pid>/environ`
  - possibly the bot's memory: `ptrace`/`/proc/pid/mem` if Yama `ptrace_scope=0`, which is the Debian default
- It also has full network access, so it can exfiltrate directly.
- **Mitigations:**
  - (a) `cmd.Env` set to a minimal list (`PATH`, `LC_ALL=C`). A nil `Env` inherits the bot's environment, so secrets would also sit in ffmpeg's own memory, where the stack-disclosure CVEs (125095, 125190, 125266, 120243, 120246) could paint them into pixels.
  - (b) `prctl(PR_SET_DUMPABLE, 0)` in the bot at startup. Same-uid children can then no longer read `/proc/<bot>/environ|mem` or ptrace it.
  - (c) A sandbox shim: re-exec the bot binary in a helper mode that sets `no_new_privs`, applies **Landlock** (read: input file, font, `/usr/lib`, `/lib`, `/usr/bin/ffmpeg`; write: the job output dir; nothing else), applies a **seccomp** filter denying `socket` (except none), `ptrace`, `process_vm_*` and `keyctl`, sets rlimits, then `execve`s ffmpeg.
    - Docker's default seccomp profile allows the Landlock syscalls (moby commit af819bf, PR #43991).
    - bwrap/nsjail need user namespaces, which Docker's default profile blocks.
  - (d) Stronger: a separate ffmpeg worker container with `network_mode: none`, `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, its own uid, and only a jobs volume (no config mount). Without a Docker socket in the bot.
  - (e) systemd: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ReadWritePaths=<data>`, `ProtectHome=read-only` (or move out of `/home`), `PrivateTmp=yes`, `PrivateDevices=yes`, `ProtectKernelTunables/Modules/Logs=yes`, `ProtectControlGroups=yes`, `RestrictNamespaces=yes`, `RestrictSUIDSGID=yes`, `LockPersonality=yes`, `SystemCallFilter=@system-service`, `SystemCallArchitectures=native`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, `MemoryMax=`, `TasksMax=`, `CPUQuota=`, `UMask=0077`.
    - The example unit has none of these.
    - Unit-level settings cover bot and ffmpeg together, so (c) is still needed to separate ffmpeg from the secrets.

### 2.6 Secrets in the download path

- `FileDownloadLink` is `https://api.telegram.org/file/bot<token>/<path>`.
- Go's `*url.Error.Error()` includes that URL. `downloadTelegramFile` wraps the error (`%w`), and callers log it (`handlers.go:210` "photo upload failed: %v"). A network error therefore writes **the bot token into logs** (journald, or Docker json-file).
- **Mitigation:** redact the URL (return a fixed message or `errors.Unwrap(urlErr)`), give the HTTP client its own timeouts, never pass that URL to ffmpeg, and never echo ffmpeg/ffprobe stderr (which contains paths) to users.

### 2.7 Temp-file, path and permission tricks

- User-controlled names give extension-driven demuxer selection (AviSynth, HLS), path traversal (`../`) and option or protocol injection.
- Predictable shared paths allow pre-planted symlinks or cross-job reads.
- A same-uid compromised ffmpeg can poison cached files for later runs.
- **Mitigations:**
  - `os.MkdirTemp` per job (0700) and `os.CreateTemp`/`O_EXCL` (0600).
  - Random names, never from Telegram.
  - Write the download, `fsync`, then `chmod 0400`.
  - Read outputs from stdout rather than predictable paths.
  - `defer os.RemoveAll(jobDir)`.
  - The cache gets a retention limit (hours, not weeks), lives apart from `bot.db`, and is keyed by bot/chat. Delete on chat purge.
  - Files uploaded to Anthropic's Files API are deleted after use.

### 2.8 Polyglots and type confusion

- A file can be valid in several formats. Probing chooses the highest score. Telegram's `mime_type`, `file_name`, `width`, `height` and `duration` are "as defined by the sender" (Bot API docs).
- **Mitigations:** Go magic-byte sniffing plus the format allowlist (fail closed: if the top-scoring demuxer isn't allowed, ffprobe errors rather than falling back). Trust only post-probe values, re-validated. Use per-type allowlists (stickers must be VP9 in WebM, ≤ 512 px, ≤ 3 s per Telegram's sticker rules).

### 2.9 LLM-specific vectors (frames and metadata as prompt injection; tool abuse)

- Text burned into frames and ffprobe tags (title, comment, handler_name) are attacker text the model will read.
- Tool arguments can be steered:
  - another chat's video reference
  - huge frame counts
  - negative or NaN times
  - repeated calls (budget burn)
- **Mitigations:**
  - Video references resolve only within (bot, chat, business connection), as opaque per-chat IDs. Never pass paths or Telegram file_ids to or from the model.
  - Validate numbers: finite, `0 ≤ start < end ≤ duration`, `1 ≤ frames ≤ N`.
  - Per-turn and per-chat budgets on ffmpeg runs and uploads.
  - Don't forward raw tags.
  - Mark frame content as untrusted in the tool result.
  - Don't echo rendered sheets back to the chat by default. That reduces the bandwidth of pixel-level memory disclosure.

---

## 3. My top 10 hardening requirements (ranked)

1. **R1: Demuxer and protocol allowlist, plus bot-controlled naming and isolation.** Critical: it stops local and cross-chat file reads (DASH and ffconcat by content, HLS by extension), libxml2/libopenmpt/libjxl exposure, and IMF/VobSub/SBaGen.
   - Both ffprobe and ffmpeg get `-protocol_whitelist file -format_whitelist mov,matroska,gif`, narrowed per media type.
   - ffmpeg gets `-f <probed>`.
   - Go magic-byte sniff.
   - Random neutral filename passed as `file:/abs`.
   - Per-job 0700 dir holding only the input.
   - Media cache separate from `bot.db`, per bot/chat, unguessable names, short retention.
2. **R2: Decoder allowlist with exact implementation names, checked at startup against `ffmpeg -decoders`.** Critical: it blocks dozens of decoder RCE and disclosure CVEs (MagicYUV, RASC, CFHD, rawvideo, VVC…), many unpatched as of 2026-11-16.
   - Video: `h264,hevc,vp8,vp9,<libdav1d|libaom-av1>,gif`.
   - ffprobe additionally gets the common audio decoders (`aac,mp3float,opus,vorbis,flac,ac3,eac3,alac,pcm_s16le`), or it fails on normal phone videos.
   - Plus `-map 0:v:0 -an -sn -dn`, no `-hwaccel`, and `-threads 2`.
3. **R3: No untrusted data in argv positions or filtergraphs.** Critical: verified that `textfile=`, `/text=` and `movie=` turn any injection into file read or whitelist bypass.
   - Constant filter template with only range-checked, Go-formatted numbers.
   - drawtext with fixed `fontfile`, `expansion=none` and a strict charset, or (preferred) labels, tiling and JPEG encoding done in Go from fixed-size raw frames.
   - Absolute ffmpeg path resolved at startup.
4. **R4: Separate ffmpeg from secrets and the network.** Critical, defence in depth for the unpatched CVE backlog.
   - `cmd.Env` minimal.
   - Bot calls `PR_SET_DUMPABLE=0`.
   - Sandbox shim: `no_new_privs` + Landlock FS allowlist + seccomp (deny socket/ptrace/process_vm) + rlimits. Or a no-network worker container with no config mount.
   - Container: `cap_drop: ALL`, `no-new-privileges`, `read_only`, `pids_limit`, `mem_limit`, `cpus`.
   - systemd: the directive set in §2.5(e).
5. **R5: Bounded resources per run and in total.** Important (DoS: HEVC PPS CPU, VP9 leak, pssh stack, pixel and timestamp bombs).
   - Context deadline, `WaitDelay`, process-group kill, `Pdeathsig`, `-timelimit`.
   - `-max_pixels 16777216`, `-max_alloc 512M`, `-max_streams 16`, `-probesize`/`-analyzeduration` 5 MB / 5 s, `-filter_threads 1`.
   - Clamp `-ss`/`-t`, `-frames:v 1`.
   - stdout via `LimitReader`, capped stderr.
   - Global semaphore, one job per chat, bounded queue.
   - Reject probed values out of range: duration, w/h, fps, stream count.
6. **R6: Version and build policy.** Important.
   - `FROM alpine:3.24` (or a digest), not `:latest`. Rebuild at least weekly.
   - Startup check: `ffmpeg -version` ≥ 8.1.3 (on 8.1.x) or ≥ 9.0.2. Refuse builds with avisynth/vapoursynth unless R1 holds. Log the build config.
   - Watch `ffmpeg.org/security.html` and the Alpine/Debian trackers; the Nov-2026 batch is still unpatched.
   - Native deployments must not rely on stale distro packages (for example Ubuntu "universe" ffmpeg).
   - Optional, strongest: a custom minimal ffmpeg (`--disable-everything --enable-demuxer=mov,matroska,gif --enable-decoder=… --enable-filter=fps,scale,format,pad,tile --enable-encoder=mjpeg --enable-muxer=image2pipe --enable-protocol=file,pipe --disable-network --disable-autodetect`). This is the Plex-style allowlist JFrog called "the only effective defense".
7. **R7: Download-path secret hygiene.** Important.
   - Redact `*url.Error` (the token currently reaches logs on network errors).
   - Dedicated HTTP client with timeouts.
   - `LimitReader` (20 MB + 1).
   - Never hand the URL to ffmpeg.
   - Never show ffmpeg stderr or paths to users.
8. **R8: Scoped, validated LLM tool.** Important.
   - Per-chat opaque video refs.
   - Finite, clamped numeric args.
   - Per-turn and per-chat budgets.
   - Frames and tags treated as untrusted.
   - Raw metadata not forwarded.
   - Raw sheets not echoed back by default.
9. **R9: File hygiene.** Important.
   - `MkdirTemp`/`CreateTemp`, 0700/0600, `O_EXCL`, inputs set read-only.
   - Outputs on stdout.
   - `RemoveAll` on every exit path.
   - Retention limit on cached media.
   - Delete Files API uploads after use.
   - Never follow symlinks in the cache.
10. **R10: Output-side surface minimisation.** Minor.
    - Encode only with `mjpeg` (avoid the PNG/APNG/GIF/WebP encoders, which have open CVEs).
    - `-map_metadata -1 -map_chapters -1`.
    - Fixed output geometry.
    - Treat exit ≠ 0 or truncated output as failure (no partial sheets).
    - Optionally `-xerror` or `-max_error_rate` after compatibility testing.

**A tested argv template** (9.0.2 local, all checks in §4 pass):
```
ffprobe -hide_banner -v error -protocol_whitelist file -format_whitelist mov,matroska,gif
  -codec_whitelist h264,hevc,vp8,vp9,libdav1d,libaom-av1,gif,aac,mp3float,opus,vorbis
  -max_streams 16 -max_pixels 16777216 -threads 1
  -show_entries format=format_name,duration:stream=index,codec_type,codec_name,width,height,pix_fmt,avg_frame_rate
  -of json file:/abs/job/input.bin
ffmpeg -hide_banner -nostdin -nostats -loglevel error -max_alloc 536870912
  -protocol_whitelist file -format_whitelist mov,matroska,gif -f <mov|matroska|gif>
  -codec_whitelist h264,hevc,vp8,vp9,libdav1d,libaom-av1,gif -max_pixels 16777216 -max_streams 16 -threads 2
  -ss <num> -t <num> -i file:/abs/job/input.bin
  -map 0:v:0 -an -sn -dn -map_metadata -1 -filter_threads 1
  -vf "fps=<num>,scale=<w>:<h>:force_original_aspect_ratio=decrease,pad=<w>:<h>:(ow-iw)/2:(oh-ih)/2,drawtext=fontfile=<abs>:expansion=none:text='00\:01':…,tile=<c>x<r>"
  -frames:v 1 -c:v mjpeg -q:v 4 -f image2pipe pipe:1
```
Plus `-timelimit`, a context deadline, a minimal Env, and the sandbox shim.

---

## 4. Checks run (local, harmless, self-made samples)

| Check | Result |
|---|---|
| `ffmpeg -version` / `-buildconf` | 9.0.2 gyan essentials. `--enable-avisynth`, libxml2, libopenmpt, libgme, libssh, libsrt, libzmq; no dav1d; 374 demuxers, 551 decoders |
| Alpine package versions (APKINDEX v3.24, edge; APKBUILD 3.24-stable/master) | 3.24 → ffmpeg 8.1.3-r0; edge → 9.0.2-r3. Configure flags listed in §0.5 |
| HLS playlist named `.mp4` / `.bin` / `.txt` / no extension | Refused: "Not detecting m3u8/hls with non standard extension" |
| HLS playlist named `.m3u8` / `.m3u` → `file:/abs/seg.ts` | **Opened and decoded the referenced local file** |
| HLS segment → `canary.txt` / `canary.ts` (text) / `data:` URI | Refused (allowed_segment_extensions; first segment fails) |
| ffconcat content in `cc.mp4` / `cc` / `cc.ffconcat` → `seg.ts` | **Detected by content and opened the sibling file.** Absolute path refused ("Unsafe file name") |
| MPD content in `dash.mp4` / no extension → `normal.mp4`, `../normal.mp4`, `file:<abs>/normal.mp4` | **All opened.** `http://127.0.0.1:9/` refused (not on `file,crypto,data`) |
| XBIN magic in `x.mp4` | Detected as xbin by content |
| `-format_whitelist mov,matroska,gif` on hls / concat / dash / xbin samples | All rejected "Format not on whitelist". mp4, webm and gif accepted. Aliases (`mp4`, `webm`) also match |
| `movie=evil.m3u8` and `movie=cc.mp4` in `-filter_complex` with the input whitelists set | **Both opened** (source filters bypass the input whitelists) |
| `drawtext=textfile=canary.txt` vs literal text | Identical PNG output: the file content was rendered |
| `drawtext=/text=nope_missing.txt` | "Error loading value for option 'text' from file": the parser opens arbitrary paths |
| `-codec_whitelist vp9` on H.264 / `h264` on H.264 | Probe and decode fail / succeed |
| `-codec_whitelist av1` on an AV1 MP4 (local) | Fails: "Codec (libaom-av1) not on whitelist 'av1'". `libaom-av1` works |
| `-codec_whitelist h264,mp3` on H.264+MP3 | Fails: default MP3 decoder is `mp3float` |
| ffprobe `-codec_whitelist h264` on H.264+AAC (also with `-select_streams v`) | **Exit 1, empty JSON**. Adding `aac` fixes it. ffmpeg with `-an` returns 0 (warnings only) |
| `-max_pixels 1000000` on a 2000×2000 H.264 file | ffmpeg and ffprobe both refuse the picture |
| `-timelimit` on Windows | "not implemented on this OS" (works on Linux via RLIMIT_CPU) |
| `-h demuxer=mov` / `-h demuxer=hls` | `enable_drefs` default false; HLS `allowed_extensions`, `allowed_segment_extensions`, `extension_picky=true` |
| Full hardened template on H.264 MP4, H.264+AAC, H.264+MP3, VP9 WebM, VP9 **alpha** WebM (512², 3 s), GIF, AV1 MP4 | All exit 0 with a valid JPEG sheet (13–26 KB). Visual check of the alpha sticker sheet OK |
| Source diff n8.1.2 → n8.1.3 → n9.0.2 (`rscc.c`, `rasc.c`, `dvbsub_parser.c`, `cfhd.c`, `lcldec.c`) and `mov.c` `mov_read_ispe` | 8.1.3 contains the 9.0-era fixes. CPE ranges over-report 8.1.3 |
| `allcodecs.c` (n8.1.3) order | native `vp9` before `libvpx-vp9`; `libdav1d` before `libaom-av1` before native `av1` |

## 5. Sources

- **FFmpeg:** <https://ffmpeg.org/security.html>; <https://ffmpeg.org/releases/>; source at tags n8.1.2, n8.1.3 and n9.0.2 on <https://github.com/FFmpeg/FFmpeg>.
- **CVE records:** `https://cveawg.mitre.org/api/cve/<ID>` for every ID cited (NVD detail pages: `https://nvd.nist.gov/vuln/detail/<ID>`).
- **Distribution trackers:** <https://security-tracker.debian.org/tracker/source-package/ffmpeg>; <https://security.alpinelinux.org/srcpkg/ffmpeg>; Alpine aports `community/ffmpeg/APKBUILD` (3.24-stable and master); <https://dl-cdn.alpinelinux.org/alpine/v3.24/community/x86_64/>.
- **PixelSmash:** JFrog, <https://jfrog.com/blog/pixelsmash-critical-ffmpeg-vulnerability-turns-media-files-into-weapons/>.
- **Past exploits of this attack class:**
  - HackerOne #237381 (neex, AVI GAB2 + HLS file read)
  - HackerOne #1062888 (Flickr HLS file read)
  - <https://ise.io/blog/archives/hlsconcat-subfile/>
  - CVE-2016-1897/1898
  - PentesterLab "SSRF via FFmpeg" (Black Hat "Viral Video")
  - Rails Active Storage CVE-2026-126186 / CVE-2026-126188
- **Docker seccomp / Landlock:** moby commit af819bf / PR #43991 (Landlock syscalls allowed in the default seccomp profile).
- **Telegram Bot API:** <https://core.telegram.org/bots/api> (getFile 20 MB limit, download URL embeds the token, sender-defined `file_name`, `mime_type` and dimensions).

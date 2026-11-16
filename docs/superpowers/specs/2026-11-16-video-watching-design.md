# Video watching: design

Date: 2026-11-16. Status: approved 2026-11-16.

## Intent

The Claude API has no video input. This feature lets a bot "watch" what users send it: videos, animated GIFs, round video messages and video stickers.

- The bot downloads the file from Telegram and builds a timestamped sheet of frames with ffmpeg.
- Claude answers from that sheet, or asks to see specific moments in more detail through one client tool, within a per-turn budget.
- On models that write progress notes, the existing progress-notes feature narrates longer looks for free.

The engine is persona-agnostic (see the README), so this is a generic capability: switched on and tuned per bot in its config, with no bot-specific assumptions in code. The busiest real deployment runs in Telegram Business chats, so everything must work there. Audio transcription is a later project; this design leaves room for it.

## Goals

1. A bot with the feature on answers questions about a video, GIF, round video message or video sticker it was sent, including questions about specific moments.
2. Claude can request either one sharp frame at a time `t`, or a denser timestamped sheet of a stretch `[start, end]`, through one tool. A per-turn budget of lookups caps the cost.
3. Follow-up questions in later turns can still refer to the video, and look at further moments while the file is retained.
4. Works in private chats, groups, forum topics and Business chats, using the existing reply target and progress-notes machinery.
5. A bot without the feature behaves exactly as today: same API requests, same Telegram calls.

## Non-goals

- Audio or speech in videos. It is a later project; leave a clean place for a transcript text block next to the sheet.
- Rendering animated `.tgs` (Lottie) stickers. They are vector animations ffmpeg cannot decode, so they keep today's emoji-only behavior.
- Files over Telegram's 20 MB bot download limit, a local Bot API server, or picking a smaller encoding from `Video.qualities`. The user considers the 20 MB cap a welcome limit on RAM and cost.
- Native video input to Claude, or an MCP server. Neither exists or fits.
- Changing how photos, voice, stickers or text behave, beyond adding a sheet to video stickers. The sticker path's existing hardcoded fallback replies are out of scope.

## Anti-goals (failure even if it technically works)

- **The model can open another chat's video.** Video references must be scoped to the chat.
- **User-facing text the bot writes itself** (hardcoded English). When the bot cannot watch something, the model is told a fact and answers in its own words.
- **Unbounded cost or disk use.** That covers runaway lookups, unbounded frames in history, and videos kept forever.
- **Breaking prompt caching for bots that don't use the feature,** or changing their requests at all.
- **A video on disk or in the Files API outliving `/clear_hard`.**
- **An ffmpeg run that can hang a turn, or reach the network or files outside its input.**

## Constraints (verified 2026-11-15/16)

### Telegram Bot API 10.4 (go-telegram/bot v1.28.0)

- **Download limit:** "bots can download files of up to 20MB in size" (getFile). `file_size` is known from the update before downloading.
- **Message types:**
  - `video` (`Video`: width, height, duration, file_size, mime_type, thumbnail, cover, qualities).
  - `animation`, described as "GIF or H.264/MPEG-4 AVC video without sound". When it is set, `document` is set too.
  - `video_note` (`VideoNote`: length/diameter, duration, file_size; round, about 1 minute or less).
  - `sticker` with `is_video` (WEBM, VP9, at most 3 s, 30 fps, 512 px, no audio, 256 KB or less). `is_animated` marks a `.tgs` Lottie sticker.
  - A video sent "as a file" arrives as `document` with a `video/*` mime type.
- **Captions** arrive in `message.Caption`. Edits to captions already update the stored text (`handleEditedMessage`).
- **Albums:** videos can be part of a media group (`MediaGroupID`).

### Claude API (2026-10-21; anthropic-sdk-go v2.9.0)

- **No video input.** Supported images are JPEG, PNG, GIF and WebP; for an animated GIF "only the first frame is used".
- **Image cost** is `⌈width/28⌉ × ⌈height/28⌉` visual tokens.
- **Resolution tiers:** high-resolution is "Claude 4.7 and later" (2,576 px long edge, 4,784 tokens). Standard is "all other models" (1,568 px, 1,568 tokens; Haiku 4.5 is standard). Images above a tier's limit are downscaled.
- **Oversized tool-result images:** the docs say these are rejected instead of downscaled, but they name only computer-use and browser-use screenshots. Not stated for custom tools. **Size every image ourselves.**
- **More than 20 images in one request:** each image must be at most 3,000 px on both sides. Images resent from earlier turns and images inside `tool_result` count toward the 20.
- **`tool_result` content** may hold `text` and `image` blocks. Base64 images are documented. Whether a Files API `file_id` works inside a `tool_result` is not documented.
- **Multiple images:** introduce each with a short text label.
- **Tool use:**
  - `tool_choice` `any`/`tool` returns 400 on Opus 5.5, Sonnet 5.5 and Fable 5.x, so only `auto` is used.
  - `strict: true` guarantees schema-valid input.
  - Tool results must directly follow the assistant message that holds the `tool_use` blocks, in one user message with all results first.
  - Thinking blocks (progress notes included) are passed back unchanged; the history is append-only within the turn (preserved thinking).
- **Prompt caching** renders `tools` → `system` → `messages`. Adding or removing a tool invalidates the cache for everything after it.

### This codebase today

- **Videos are ignored.** A video, GIF, round video message or video-as-document is stored as an empty-text message and ignored, and its caption is dropped. Video stickers reply from the emoji only (`generateStickerResponse`).
- **Photos** are uploaded to the Files API and stored as `Message.ImageFileIDs`. They are resent in history as image blocks (`contentBlocksForMessage`) and deleted from the Files API by `/clear_hard` (`hardDeleteScope`). Albums are buffered for 1 s and become one turn (`album_buffer.go`).
- **The model loop** in `getAnthropicResponse` follows `pause_turn` and a file-not-found self-heal. It has **no client-tool loop**. `replyAssembler` already treats `tool_use` as a tool-call block, so progress notes before a client tool call already work.
- **Media turns** (`beginTurn`) wait for a running turn and are never restarted. `runTurn` handles replies, notes, errors, Stop and restarts.
- **Reply targeting:** `replyTarget` carries chat, Business connection and forum topic to every send.
- **Docker:** the runtime image is `alpine:latest` with no ffmpeg. `./data` is a mounted volume.
- **ffmpeg check:** local ffmpeg 9.0.2 has `drawtext` (libfreetype), `tile`, `fps`, `select`, `scale` and libvpx. One command built a 12-frame, 934×1224 timestamped sheet from a 37 s 720×1280 clip in 3.8 s. Without a font file, `drawtext` logged fontconfig errors and fell back to a default font.

## Approaches considered

- **A. `seek(t)`, one frame per call (the user's first idea).** Simple for the model. Scanning a stretch takes many round trips, each resending the conversation, so it is slower and costlier.
- **B. `view_frames(start, end, count)`, one tool for both a single sharp frame and a zoomed-in mini-sheet. Picked.** It needs the fewest round trips for both kinds of question. Every call returns exactly one image, so a budget counted in lookups maps straight to cost.
  - Correction after the design review: an earlier draft said "frames" here. That was wrong, because a 12-frame mini-sheet costs about the same tokens as one frame.
- **C. No tool: many frames sent up front.** Easiest to build, but every video pays for every frame, nothing can zoom in, and history quickly hits the 20-image rule.
- **Rejected outright:**
  - MCP: Anthropic's connector needs a public HTTP server, and the files live on the bot's disk.
  - The sheet alone: it can't answer questions about a specific moment.

The user picked B.

## Design

### Switch and config

A new optional `video` block in a bot's config. If the block is absent, nothing below happens and requests stay byte-identical. Keys and defaults are my proposals; the builder may rename them for consistency with existing keys:

| Key | Default | Meaning |
|---|---|---|
| `max_duration_seconds` | 600 | Longer videos are not watched; the model is told so. |
| `sheet_frames` | 12 | Frames on the first sheet (grid 4×3). Video stickers always use 6 (3×2). |
| `max_lookups` | 6 | `view_frames` calls (one image each) allowed in one turn, counting every call, including several in one response. Cost follows images, plus each later loop step resending the turn's earlier images, so this is the cost knob. |
| `max_frames_per_lookup` | 12 | Cap on `count` per call. It controls how dense a mini-sheet is, not its cost. |
| `retention_hours` | 24 | How long the downloaded file is kept on disk for follow-up lookups. |

- **Validation at boot:** positive integers within sane bounds (builder sets the bounds).
- **ffmpeg missing:** if `video` is set but `ffmpeg` or `ffprobe` is not on `PATH`, log one boot advisory and treat the feature as off for that bot.
- **Advisory when on:** log one line, like the other features, mentioning that lookups narrate only on models that write progress notes.

### Which messages are watched (bot has `video`)

| Incoming | Handling |
|---|---|
| `video`, `animation`, `video_note`, `document` with `video/*` or `image/gif` mime | Download (20 MB or less), probe, build a 12-frame sheet, keep the file for lookups. |
| `sticker` with `is_video` | Stays on today's sticker path (my call; see below). Download, build a small 6-frame sheet (3×2, long edge 512 px or less, about 200–300 tokens), no lookups, file not retained. |
| `sticker` with `is_animated` (`.tgs`) | Unchanged (emoji only). |
| Too big (`file_size` over 20 MB), too long, undecodable, or ffmpeg fails | No sheet. The stored message gets a short factual text for the model, e.g. `[Video 2:13, 48 MB: not watched — over the 20 MB limit]`. The model then answers in its own words. |

- **Turn behavior:** a watched video, GIF, round video message or video document is a media turn like a photo, run through `runTurn` with notes, drafts and Stop. It cancels any pending text debounce, waits for a running turn, is never restarted, counts as one message for rate limits, and shows the typing indicator during download and ffmpeg.
- **Video stickers keep the sticker path (my call).** Today that path is also a committed media turn (`beginTurn`), with no tool and no notes. The only change is that the stored sticker message also carries the small sheet, so the model sees the animation, not just the emoji. The emoji still goes into the text.
  - The reply style stays the owner's choice, as today: the sticker path's emoji flag appends the bot's `system_prompts.respond_with_emojis` rule if the config sets one.
  - The sticker path's existing hardcoded fallback replies are untouched (see Non-goals).
- **Albums:** videos that arrive in a media group are collected by the existing album buffer together with photos, and become one turn (my call).
- **Caption:** stored as the message text, as for photos.

### What the model sees

- **The user message** holds a short label text block, then the sheet image, then the caption text. The label is model-facing English, not shown to users, for example: `Video v3 (0:37, 720×1280, 12 frames every 3.1 s; lookups available until 2026-11-17 14:02)`. Its last part says when lookups end, or that they are unavailable (video stickers, expired files).
- **The sheet:**
  - Each tile has its timestamp burned in (`drawtext`, `%{pts:hms}`) on a dark box.
  - Frames are spaced evenly over the duration.
  - **Sizing rule for every image the bot sends** (first sheets, lookup frames, lookup mini-sheets): it fits both standard-tier limits, a long edge of 1,568 px or less and 1,568 visual tokens or less (`⌈w/28⌉ × ⌈h/28⌉`). No model then downscales it, and the burned-in timestamps stay sharp. One sizing function serves all of them (my call). On high-resolution models, portrait sheets come out about a fifth smaller than they could be; that is accepted.
  - The sheet is uploaded to the Files API and stored in the message's `ImageFileIDs`, exactly like a photo. It stays in history, follows the existing memory window, and is removed by `/clear_hard` through the existing code path.
- **The video ID** (`v3`) is short, unique within the chat, and is the only handle the tool takes.

### The tool

```
view_frames(video: string, start: number, end: number, count: integer)
```

- **When it is sent:** with `strict: true` and `tool_choice` `auto`, on every request of a bot that has `video` on, whether or not a video is in memory. That keeps the tool list, and so the prompt-cache prefix, stable (my call). Bots without `video` never send it.
- **Description:** in English for the model (not user-facing). It explains:
  - `count` 1 returns one full-detail frame at `start` (`end` ignored);
  - `count` above 1 returns a timestamped sheet of `count` frames spread over `[start, end]`;
  - times are in seconds;
  - the remaining lookups are reported in every result.
- **Input handling:** validated and clamped (the times to `[0, duration]`, `count` to `[1, max_frames_per_lookup]`).
- **The video must belong to the same chat.** Otherwise, or if the file has expired, the call returns an error result (`is_error: true`) with a factual text, and the model carries on with what it has.
- **Result:**
  - a text block (`frames at 0:40.0–0:50.0, lookups left: 4`) plus one image;
  - a single frame or a mini-sheet, sized by the sizing rule above, JPEG, **inline base64**, because `file_id` inside `tool_result` is undocumented;
  - these images exist only within the turn and are never stored in chat memory or the database.
- **Budget exhausted:** once `max_lookups` calls have run this turn, every further call returns an error result saying so. That includes calls that arrive together in one response.

### The tool loop

- **Where:** inside `getAnthropicResponse`, extending its existing loop (not the SDK's tool runner), so the reply assembler, progress notes, `pause_turn` and self-heal keep working.
- **Each step:**
  1. When a response ends with `stop_reason` `tool_use`, append the assistant message unchanged (`msg.ToParam()`).
  2. Run every `view_frames` call in it, sequentially, in order.
  3. Append one user message containing only the `tool_result` blocks, in the same order.
  4. Continue.
- **Unknown tools** get an error result.
- **Step cap:** a hard cap on loop iterations per turn (builder's call, e.g. 8), separate from the `max_lookups` budget, guards against a model that keeps calling. At the cap, the turn ends with what it has, like today's `pause_turn` cap.
- **Notes:** the progress notes the model writes before each call are shown by the existing status line; nothing new is needed there.
- **Within a turn:** tool results and the assistant tool-use messages are never written to chat memory. Only the final answer is stored, as today.

### Video storage and lifetime

- **Location:** retained files live under the data directory (e.g. `data/videos/<bot>/<chat>/<id>.<ext>`). The video record (ID, chat, kind, path, duration, size, dimensions, expiry) is stored in the database. A new table or columns is the builder's call, migrated with GORM AutoMigrate.
- **Expiry:** a sweeper deletes expired files and marks their records unavailable, at startup and periodically (builder's call, e.g. hourly).
- **`/clear_hard`** deletes the chat's retained files and records immediately, alongside the existing Files API deletes. `/clear` (soft) leaves them to expire, matching how photos' Files API objects survive a soft clear.
- **Video stickers** are processed from a temp file and not retained.

### ffmpeg hardening (sensible, not paranoid)

The input is a Telegram-hosted file:
- Probe it first with `ffprobe`. Accept only an allowlist of container formats (mp4/mov family, matroska/webm, gif) with a video stream.
- Run every ffmpeg/ffprobe call:
  - with a timeout tied to the turn's context (builder's call, e.g. 60 s for a sheet, 20 s for a lookup);
  - with `-nostdin`;
  - with `-protocol_whitelist file` so a crafted file cannot make it read URLs or other files;
  - with input and output paths the bot generated.
- Never build a command from user text. gosec's G204 on these `exec.Command` calls is suppressed only with a reasoned `#nosec`, citing the fixed binary and generated arguments.
- Video stickers (VP9 with alpha) decode with libvpx and are placed on a neutral background. Builder's call.

### Fonts and Docker

- The Docker runtime stage installs `ffmpeg` and one font package, and the bot passes `drawtext` an explicit font file path. A config/env override is the builder's call.
- The builder confirms Alpine's ffmpeg build has `drawtext` (libfreetype).
- Native deployments need `ffmpeg` and `ffprobe` on `PATH`. The README says so.

### README

A **Video watching** section covering:
- the config block and defaults;
- which messages are watched;
- costs (one sheet in history per video, plus lookups within the budget);
- retention and `/clear_hard`;
- ffmpeg and font requirements;
- that lookups are narrated only on models that write progress notes;
- a suggested `custom_instructions` line for owners who want the bot to look closer before answering.

## Done means

- **Off:** a bot without `video` sends identical requests (no tool) and makes identical Telegram calls. Tests prove the request body has no `view_frames` tool.
- **Each type watched:** videos, GIFs, round video messages, video documents and video stickers each produce a stored message with a timestamped sheet in `ImageFileIDs` and the label text. An ffmpeg-backed test runs on small generated clips and skips when ffmpeg is absent. CI installs ffmpeg.
- **Not watched:** an over-limit, too-long or undecodable file produces only the factual label, and no hardcoded user-facing text is sent.
- **Tool loop:** with an httptest SSE stub:
  - a `view_frames` call is answered with a correct `tool_result` (text plus base64 image within both the 1,568 px and 1,568-token limits);
  - the loop continues and the final answer is delivered;
  - nothing from the tool exchange reaches chat memory;
  - budget exhaustion (including several calls in one response), the step cap, an unknown video, another chat's video and an expired video each give an error result, and the turn still ends with an answer;
  - a video sticker's stored message carries the small sheet and the emoji, and its reply still goes through the sticker path.
- **Retention:** files and records are deleted by the sweeper after expiry and by `/clear_hard` at once.
- **Clean checks:** `go test -race`, `go vet`, gosec and golangci-lint are clean, and the jscpd gate shows no new clones.

## Left to the builder

- File layout. For example, `video.go` (intake and records), `ffmpeg.go` (probe, sheet, lookup images) and `video_tool.go` (tool definition and execution). Also the exact DB schema.
- Exact defaults and bounds for timeouts, the step cap, the sweeper interval and config validation.
- The label wording, kept short and factual. The tool description text.
- How video stickers' alpha is flattened, and the font path and package.
- Whether a lookup sheet reuses the first-sheet code path with different parameters. It should, to avoid duplication.

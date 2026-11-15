# Progress notes during tool use: design

Date: 2026-11-15. Status: approved 2026-11-15.

## Intent

When a turn involves tool calls (the tibik-docs MCP server, web search and fetch, and later the video-watching seek tool), the user should see short notes that Claude writes about what it is doing, so a long turn does not read as silence. The model writes these notes itself, in its own words and the conversation's language. The bot never hardcodes or translates status strings.

This is the first of two projects. The second, video watching (storyboard plus a seek tool loop), will produce long multi-step turns and reuses this feature as is. Build it as a general "show a note" facility, not something tied to MCP.

## Goals

1. During a turn, show the model's notes as one status message that is edited in place as new notes arrive, then removed once the answer is sent.
2. Work in private chats, basic groups, supergroups (including forum topics) and Telegram Business chats.
3. The phone makes a sound for the answer, not for the status line.
4. Opt in per bot through config. A bot without it sends byte-identical API requests and makes exactly the same Telegram calls as today, apart from the forum-topic fix below, which applies to every bot.

## Non-goals

- Making the model write notes. Whether and how often it writes them is steered through `system_prompts` (config), not code.
- Hardcoded or translated status text, or a replacement for the typing indicator. The indicator stays as it is.
- Saving notes to the database or chat memory. Only the answer is stored, as today.
- Any video, ffmpeg or seek-tool work (project 2).
- A per-model allowlist. See Constraints for why none is needed.

## Anti-goals (failure even if it technically works)

- Status messages left behind in the chat. The one tolerated case is a delete that fails mid-turn, which is logged.
- A notification for "Checking the docs…" while the answer arrives silently.
- Notes changing what the bot remembers or what the next turn sees.
- Breaking today's restart, Stop button, debounce or draft behavior, or making the Android composer lock (draft on screen) earlier than it does today.
- Extra Telegram calls on turns where the model writes no notes.
- Hiding substantive answer content in a status line that later disappears.

## Constraints (verified 2026-11-15)

### Claude API

- **Which models write notes:** "On Claude Fable 5.2, Claude Mythos 5.2, Claude Fable 5.1, Claude Mythos 5.1, Claude Opus 5.5, Claude Sonnet 5.5, and Claude Fable 5, the model can write a progress update between tool calls." Not Sonnet 5, Haiku 5.5 or Haiku 4.5.
- **How to get them:** `thinking: {type: "adaptive", display: "updates"}` plus the beta header `thinking-display-updates-2026-08-18`. Without the header the request returns a 400. SDK v2.9.0 already has both: `anthropic.BetaThinkingConfigAdaptiveDisplayUpdates` and `anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18`. `display` is invalid with thinking disabled.
- **Shape:** notes arrive as `thinking` blocks. Under `"updates"`, reasoning blocks have empty text, and "any `thinking` block with non-empty text is a progress update." A note streams as `content_block_start` (thinking), then one or more `thinking_delta` events (text can arrive in chunks), then `signature_delta`, then `content_block_stop`. "At most one progress update precedes each tool call, and the model can skip any of them." A note "sits immediately before the `tool_use` or `server_tool_use` block it introduces." The docs never mention MCP `mcp_tool_use` blocks.
- **Short notes are plain text on Sonnet 5.5:** "Notes longer than a sentence or two come back as progress-update `thinking` blocks. Shorter remarks stay `text`."
- **Interrupted responses:** "its last block can be a progress-update block standing in for the work the model hadn't completed", with the exact text `This part of the response was interrupted before it finished.` This must never be shown.
- **Passing back:** thinking blocks, notes included, go back to the API unchanged within the turn. The existing `pause_turn` continuation (`msg.ToParam()`) already does this, and the bot does not send thinking blocks across turns.
- **Cost:** notes count as output tokens at full length.
- **Live tests (2026-11-15, tibik_bot's real config):**
  - Sonnet 5.5 accepted the setting. In one run it called tibik-docs twice and wrote no note. The turn took 15 s, both MCP calls finished within 4 s, and the longest silence was about 7 s of reasoning before the answer.
  - Haiku 5.5 accepted the setting without an error and wrote no notes.
  - So turning the feature on cannot break a bot that `/set_model` moves to Haiku 5.5. Sonnet 5 is untested. Haiku 4.5 does not support adaptive thinking at all, so it can't be combined with this setting.
  - If some model does reject the setting, owners and admins see the raw API error. That is the README's existing behavior for thinking modes.

### Telegram Bot API 10.4 (go-telegram/bot v1.28.0 has every method and field below)

- **sendMessage** takes:
  - `business_connection_id`.
  - `message_thread_id`, "for forum supergroups and private chats of bots with forum topic mode enabled only".
  - `disable_notification`: "Sends the message silently. Users will receive a notification with no sound."
- **editMessageText** takes `chat_id` + `message_id`, or `business_connection_id`, but no thread ID. Text must be 1–4096 characters. The 48-hour edit limit applies only to "business messages that were not sent by the bot". Sending unchanged text is undocumented (in practice Telegram returns a "message is not modified" error), so never send unchanged text and treat that error as harmless.
- **deleteMessage:** "Bots can delete outgoing messages in private chats, groups, and supergroups", and only messages under 48 hours old. It has no `business_connection_id`.
- **Business chats:**
  - Delete with `deleteBusinessMessages(business_connection_id, message_ids)`, which "Requires the can_delete_sent_messages business bot right".
  - Sending and editing need `can_reply`: "send and edit messages in the private chats that had incoming messages in the last 24 hours".
  - Rights come from `getBusinessConnection` (`BusinessConnection.Rights`), and `business_connection` updates report changes.
- **Drafts:** private chats only, with a 30-second preview, and "The draft will still disappear after a short time or if the bot sends a message." So sending the status message clears a draft that is on screen.
- **Rate limits:** about one message per second per chat, and 20 messages a minute in groups. Edit limits are undocumented.

### This codebase today

- Thinking blocks are only logged ([anthropic.go:410-418](../../../anthropic.go)). `thinking_display` accepts only `summarized` and `omitted` ([config.go:258-268](../../../config.go)). An invalid config is skipped at startup, so the bot does not start.
- In segment mode, each completed text block is sent at once as its own message (`onSegment`). In draft mode, text streams into the draft (`onProgress`) and the joined text becomes the final message.
- A turn becomes "committed", meaning it can no longer be restarted, once the user has seen output ([turn.go:151](../../../turn.go)).
- Nothing in the bot passes `message_thread_id` today, so replies in forum topics do not target the topic.
- `TelegramClient` ([telegram_client.go](../../../telegram_client.go)) has no edit, delete or business-connection methods.

## Approaches considered

- **A. One status message, edited in place, removed after the answer. Picked.** It works in every chat type (the edit method accepts a Business connection ID), leaves nothing behind, and lets the answer notify. Cost: new Telegram client methods, plus a rights check in Business chats.
- **B. Notes inside the streaming draft.** Rejected. Drafts exist only in private chats with `stream_drafts` on, and tibik_bot has it off. It would also lock the Android composer for the whole tool phase, which the README deliberately avoids.
- **C. Each note as its own permanent message.** Rejected. Notes pile up for good, and they eat into the 20-messages-a-minute group limit.

The user picked A, on the condition that it was checked against both sets of docs for every chat type (done above). After the design review, the user also chose two follow-ups:
- No notes in Business chats where the bot lacks the delete right, instead of turning the status message into the answer. Business chats always send answers part by part, so that fallback had nothing left to turn into.
- A forum-topic fix that covers every reply, not just the turn's own sends.

## Design

### Switch

- `thinking_display: "updates"` turns the feature on. It is valid only with `thinking: "adaptive"`, the same rule as the other display values. There is no separate key, because the API setting and the feature are the same thing.
- With it set, requests carry `display: "updates"` and the beta header, next to the MCP header where present.
- At boot, log one advisory line, like the existing ones: notes appear only on models that write them (Opus 5.5, Sonnet 5.5, Fable 5.x), and the system prompt can ask for them.
- With any other value, nothing below happens and requests stay byte-identical to today.

### What counts as a note

Only when the switch is on:

1. A `thinking` block whose streamed text is non-empty, after trimming, is a note. The trigger is the block's stop event.
2. A `text` block immediately followed by a tool-call block (`tool_use`, `server_tool_use` or `mcp_tool_use`) is a note if it is short, under about 280 characters (my call). A longer text block before a tool call stays part of the answer, exactly as today. This guards the "don't hide substantive content" anti-goal and relies on Sonnet 5.5 sending only short remarks as text.
3. A block whose trimmed text is exactly `This part of the response was interrupted before it finished.` is never shown.

Rule 2 means a text block is final only once the next block starts. In segment mode, hold a completed text block until the next block starts or the response ends. If a tool call follows and the block is short, it becomes a note. Otherwise it is delivered as a segment, as today. A response that ends with `pause_turn` continues in the next request, so its first block decides.

A text block that becomes a note is removed from the turn's reply text. It is neither sent as a segment, part of the final message, nor stored in memory.

Each note is logged at INFO as `[note] …`, truncated like the `[thinking]` lines.

### The status message

A turn shows at most one status message at a time, owned by a small controller per turn. Like `draftStream`, it has a single sender goroutine, so Telegram calls are sequential and never run on the model's stream goroutine.

- **First note:** send a new message.
  - It goes to the same chat, with the turn's Business connection ID and, when the incoming message was in a topic, its `message_thread_id`.
  - It is always sent with `disable_notification: true`.
  - It is sent only after the note has waited about 1 second (my call), to avoid a send-then-delete flicker. The waiting note is dropped if, during that second, answer text reaches the user (in draft mode: starts streaming into the draft) or the turn ends.
- **Later notes:** edit the message in place, at most about once per second, always to the newest note. Skip the edit if the text is unchanged. Treat "message is not modified" as success.
- **When answer text reaches the user mid-turn (my call):** the status message never sits above answer text the user has already read.
  - **Segment mode:** right after an answer segment is sent, delete the status message. If more tool calls follow, the next note starts a new status message at the bottom.
  - **Draft mode:** answer text is not a real message until the end, so the status message stays and is deleted after the final message. A note still waiting when answer text starts streaming into the draft is dropped. Once the first block of answer text is final (it can no longer turn into a note), later notes in that turn are logged but not shown, because sending a message would clear the draft mid-answer.
- **Note length:** clamp to 4096 characters. Notes are normally a sentence or two.

### End of turn

No status message outlives its turn, except when a delete fails, which is logged.

- **Answer:** send the answer as today: segments, or the final message in draft mode. Then delete the status message. Sending first avoids a visible gap and means a failed delete never costs the user the answer.
- **Error:** delete the status message, then send the error reply as today.
- **Restart (a newer message or an edit arrives):** delete the status message when the abandoned turn ends. The rerun creates its own if it writes notes. Reusing the old message in the rerun instead is allowed (builder's call), as long as nothing is left behind.
- **Stop button:** delete the status message, then deliver the partial answer as today.

### Which turns show notes (my call)

- **Text and photo turns** (both run through `runTurn`) show notes.
- **Voice turns** show notes too, because they are the longest turns in the bot (transcription, model, speech synthesis). The status message is shown during the model call and deleted after the audio, or its text fallback, is sent.
- **Sticker replies** do not: they are emoji-only and don't use tools.

Rule 2 (turning short pre-tool text into a note) applies in every turn that shows notes, voice included: a short line before a tool call goes to the status message and is not spoken or stored. Turns that show no notes (sticker replies, and every turn when the switch is off) keep such text in the reply exactly as today.

### Notes and restarts

A note does not commit the turn. A message that arrives while only notes have been shown can still restart the turn, as it can today during MCP calls. Exception: in draft mode, a short pre-tool text block may already have streamed into the draft before it turns out to be a note. Leaving that turn committed is acceptable.

### Draft mode

- The answer streams into the draft as today.
- Sending the first status message clears an on-screen draft. That is fine: the draft text no longer contains the note (rule 2 removes it), and the draft reappears with the next answer text.
- The status message is never shown inside the draft.
- The composer lock does not get earlier, since the status message is an ordinary message.

### Business chats

Business chats never use drafts, so answers there always go out part by part.

- Before sending the first status message in a Business chat, look up the connection's rights with `getBusinessConnection`. Cache them per connection ID with a short lifetime, about 10 minutes; also refreshing on `business_connection` updates is the builder's call.
- **With `can_reply` and `can_delete_sent_messages`:** the flow above, deleting with `deleteBusinessMessages`.
- **Without `can_delete_sent_messages` (user's decision):** no notes in that chat. The turn keeps only the typing indicator, exactly as today, and notes are logged but never sent. The README tells Business owners to grant the bot the right to delete its sent messages if they want notes.
- **If the rights lookup fails (my call):** treat it as no delete right, so no notes, and log it.
- **If a delete fails anyway** (for example, rights changed mid-turn): leave the message and log it.
- **Without `can_reply`:** the bot cannot answer at all, which is today's behavior and unchanged. No notes are sent either.

### Forum topics (user's decision: every reply)

When the incoming message has `is_topic_message`, every reply to it carries its `message_thread_id`. That covers:
- **The turn's own sends:** status message, answer segments, final message, error replies, chat actions and drafts.
- **Every other reply path:** command replies (`/stats`, `/whoami`, `/clear`, `/clear_hard`, `/set_model`), rate-limit notices, voice replies (`SendAudio`) and their text fallback, sticker replies, and photo or upload error messages.

This fixes a bug that exists today, independent of notes, so it applies to every bot whether or not the switch is on. It touches:
- `sendResponse` and its roughly 30 call sites;
- `sendOneSegment`, `sendLongMessage` and `startChatAction`;
- the draft sender.

Passing one small reply target (chat ID, Business connection ID, thread ID) instead of separate arguments is the builder's call. The ID travels on `turnRequest`, and through the intake and album buffers, which already keep the latest message's metadata.

A message outside a topic never gets a thread ID. Telegram accepts the parameter only in forum supergroups and in private chats with topic mode.

Out of scope: chat memory and debounce stay per chat, not per topic, as today. A burst across two topics within the quiet window coalesces into one turn answered in the latest message's topic.

### Telegram client

Add these to `TelegramClient` and `MockTelegramClient`, keeping the existing function-field mock style:

- `EditMessageText`
- `DeleteMessage`
- `DeleteBusinessMessages`
- `GetBusinessConnection`

### Prompt nudge (config, after deploy)

Add to the README, as a suggestion for bot owners: a line in `custom_instructions` such as "Before you call a tool, write one short line in the user's language saying what you're about to check." The user will try it on tibik_bot after deploying. One live run without it produced no notes.

## Done means

- **Config:** `thinking_display: "updates"` loads with `thinking: "adaptive"` and is rejected without it. Other values behave as today.
- **Zero-cost when off:** a turn where the model writes no notes makes exactly the Telegram calls it makes today, with no status message and no rights lookup. Covered by tests with the mock client.
- **With notes:** a turn makes one silent status message, throttled edits with the newest note, the answer as new messages, then a delete. Covered for segment mode and draft mode.
- **Interleaving:** in segment mode, a turn shaped like note → tool → answer segment → note → tool → answer deletes the first status message after the first segment and shows the second note in a new one below it. In draft mode, a note after the first block of answer text is final is logged, not shown.
- **Voice turns:** the status message shows during the model call and is gone after the audio or text fallback. Sticker replies are unchanged.
- **Business:** with the delete right, `deleteBusinessMessages` is used. Without it, or when the rights lookup fails, no status message is sent and the turn's Telegram calls match today's, apart from the rights lookup itself.
- **Forum topics:** every reply to a topic message, on every path listed above, carries its thread ID. Messages outside topics never send one.
- **Restart, Stop, error and the interrupted-placeholder paths** leave no status message behind.
- **Classification:** short text before a tool call becomes a note; longer text before a tool call, and text not followed by a tool call, stays in the answer.
- **Checks:** `go test -race ./...`, `go vet`, golangci-lint and gosec are clean. The README documents `"updates"`, the status-message behavior, the Business rights note, and the prompt nudge.

## Left to the builder

- File layout. For example, a `status.go` next to `draft.go`, and pulling the stream's block classification out of `streamMessages` into a function testable without the SDK stream.
- Exact values: note length threshold (about 280 characters), first-send delay (about 1 s), edit throttle (about 1 s), rights-cache lifetime (about 10 minutes).
- Whether a restarted turn reuses or deletes the previous status message.
- Whether to refresh the rights cache on `business_connection` updates.
- How notes reach the controller: a third callback next to `onSegment` and `onProgress` is the obvious fit.

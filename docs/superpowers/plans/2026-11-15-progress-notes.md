# Progress Notes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show Claude's own between-tool-call notes as one silent Telegram status message that is edited in place and removed once the answer is sent, and make every reply land in the forum topic it answers.

**Architecture:**
- A `replyTarget` (chat, Business connection, topic) replaces the loose `chatID`/`businessConnectionID` pairs on every send path.
- A per-turn `replyAssembler` decides, block by block, what is answer text and what is a note.
- A per-turn `statusLine` runs a single sender goroutine that sends, edits and deletes the status message.
- `replyOutput` connects the two for text and photo turns, and the voice handler uses the status line directly.

**Tech Stack:** Go 1.27.4, `github.com/anthropics/anthropic-sdk-go/v2` v2.9.0 (beta streaming Messages API), `github.com/go-telegram/bot` v1.28.0, GORM + SQLite, testify.

**Spec:** `docs/superpowers/specs/2026-11-15-progress-updates-design.md`

## Global Constraints

- **Dependencies:** none added. `go.mod` stays at anthropic-sdk-go/v2 v2.9.0 and go-telegram/bot v1.28.0.
- **The switch:** `thinking_display: "updates"`, valid only with `thinking: "adaptive"`. With it off, API requests are byte-identical to today and Telegram calls are unchanged, apart from the topic fix.
- **Requests with notes on** send `thinking: {type: "adaptive", display: "updates"}` and the beta `anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18`.
- **Text shown:** no user-facing status text is hardcoded. The bot only shows text the model wrote.
- **Interrupted placeholder:** never show the exact text `This part of the response was interrupted before it finished.`
- **Short text as a note:** a text block immediately followed by a `tool_use`, `server_tool_use` or `mcp_tool_use` block is a note when it has at most `maxTextNoteRunes = 280` runes after trimming.
- **Status message timing:** at most one per turn at a time.
  - Every status message is sent with `DisableNotification: true`.
  - First send `defaultStatusFirstDelay = 1 * time.Second` after the note.
  - Edits at most once per `defaultStatusEditInterval = 1 * time.Second`.
  - Text clamped to `telegramMessageLimit` (4096) runes.
- **Business chats:** notes only when the connection has `IsEnabled`, `Rights.CanReply` and `Rights.CanDeleteSentMessages`.
  - Successful lookups are cached for `businessRightsTTL = 10 * time.Minute`, measured with `b.clock`.
  - A failed lookup means no notes and is not cached.
- **Topic ID:** set only when the incoming message has `IsTopicMessage`. It applies to every reply on every path, for every bot.
- **Storage and restarts:** notes are never stored in the database or chat memory, and never mark a turn committed.
- **Test names:** a test name with underscores carries the repo's trailing comment `//NOSONAR go:S100 -- underscore separation is idiomatic in Go test names`.
- **Commits:** stage files by name and never stage `go-telegram-bot.exe`. End every commit message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **Checks that must stay clean:** `go test -race -count=1 ./...`, `go vet ./...`, gosec v2.30.0 and golangci-lint v2.15.0.
- **Duplication check before every commit (Tasks 2–6), with jscpd 5.4.3:**
  - **Gate, must exit 0:** `jscpd --format go --ignore "**/*_test.go" --similarity --baseline-from-ref HEAD --fail-on-new-clones -r console .`. It compares the uncommitted work against the previous commit and fails on any new exact, renamed or structurally similar clone. Fix a new clone by sharing the code, never by raising limits or adding ignores.
  - **Semantic report, non-blocking:** `jscpd --format go --ignore "**/*_test.go" --semantic --semantic-scope same --kind semantic -r console .`. It is non-blocking because jscpd's docs say to keep the experimental semantic mode out of build-failing steps. It runs the local CodeRankEmbed model and takes about 40 s. Expected: `No duplicates found.` Any reported pair involving the task's files is either merged or explained in the task's report.
  - **Test files are left out of both:** table-driven tests repeat by design. They had 23 clones, 4.9% of lines, on 2026-11-15.
  - **Baseline on 2026-11-15:** production code has 3 clones (0.74% of lines) and 0 semantic clones.

## Review Focus

1. **A note arriving after the turn's status line has closed** (the stream is still emitting during teardown) makes no Telegram call. Test: Task 4, `TestStatusLine_NoteAfterCloseIsIgnored`.
2. **A status send still in flight when the turn ends** returns a message ID, and that message is deleted. Test: Task 4, `TestStatusLine_InFlightSendDeletedOnClose`.
3. **Multibyte notes near the 280 limit** (Cyrillic, emoji) are counted in runes, not bytes. Test: Task 5, `TestReplyAssembler_RuneBoundary`.
4. **An edit to a message in a forum topic** that restarts the running turn carries the topic ID into the rerun. Test: Task 2, `TestHandleEditedMessage_TopicKeepsThreadID`.
5. **A Business connection that is disabled** (`IsEnabled: false`) never shows notes, even if its rights look complete. Test: Task 4, `TestStatusLine_BusinessDisabledConnectionShowsNothing`.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `telegram_helpers.go` | `replyTarget`, `targetFor` | 2 |
| `bot.go`, `handlers.go`, `typing.go`, `draft.go`, `turn.go`, `intake_buffer.go`, `album_buffer.go` | send paths take a `replyTarget` | 2 |
| `config.go` | `ThinkingDisplayUpdates`, `NotesEnabled`, validation, advisory | 3 |
| `anthropic.go` | `buildMessageParams` (extracted), notes beta, assembler-driven streaming | 3, 5 |
| `telegram_client.go`, `telegram_client_mock.go` | edit, delete, business delete, business connection | 4 |
| `status.go` (new) | `statusLine`, `businessNotesAllowed` | 4 |
| `reply_assembler.go` (new) | `replySinks`, `replyAssembler` | 5 |
| `draft.go`, `turn.go`, `handlers.go` | notes wired into `replyOutput`, `runTurn`, voice | 6 |
| `README.md` | topic note (Task 2); progress notes section (Task 6) | 2, 6 |

Tests: `topic_test.go`, `status_test.go`, `reply_assembler_test.go`, `anthropic_stream_test.go` and `status_turn_test.go` are new. Existing test files get their call sites updated.

---

### Task 1: Commit the dependency update

The update is already in the working tree and passing (`go.mod`, `go.sum`, `.github/workflows/go-ci.yaml`). Every commit in this plan goes directly to `main`; the user asked for no feature branch. The starting commit is `401b263`.

- [ ] **Step 1: Confirm the branch:** `git branch --show-current` prints `main`, and `git rev-parse --short HEAD` prints `401b263`.
- [ ] **Step 2: Verify:** `go build ./... && go vet ./... && go test -race -count=1 ./...`. Expected: `ok  github.com/HugeFrog24/go-telegram-bot`.
- [ ] **Step 3: Check the duplication tooling.**
  - `jscpd --version` prints `jscpd 5.4.3`. Otherwise run `npm install -g jscpd@5.4.3`.
  - If the semantic model is not cached yet, run `jscpd --semantic-download` (a one-time 548 MB download).
  - Run both commands from Global Constraints. Expected: the gate exits 0, the token pass reports the baseline of 3 clones, and the semantic report prints `No duplicates found.`
- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum .github/workflows/go-ci.yaml
git commit -m "chore: bump anthropic-sdk-go to v2.9.0, go-telegram/bot to v1.28.0, CI Go to 1.27.4"
```

---

### Task 2: Reply targets and forum-topic IDs on every reply

**Files:**
- Modify: `telegram_helpers.go`, `bot.go`, `handlers.go`, `typing.go`, `draft.go`, `turn.go`, `intake_buffer.go`, `album_buffer.go`, `README.md`
- Test: create `topic_test.go`. Update call sites in `typing_test.go`, `draft_test.go`, `intake_buffer_test.go`, `handlers_test.go` and `edit_stop_test.go`.

**Interfaces:**
- Produces:
```go
type replyTarget struct {
	chatID               int64
	businessConnectionID string
	threadID             int // forum topic; 0 outside topics
}
func targetFor(msg *models.Message, businessConnectionID string) replyTarget
func (r turnRequest) target() replyTarget // turnRequest gains field: threadID int

func (b *Bot) sendResponse(ctx context.Context, to replyTarget, text string) error
func (b *Bot) sendOneSegment(ctx context.Context, to replyTarget, text string) error
func (b *Bot) sendLongMessage(ctx context.Context, to replyTarget, text string) error
func (b *Bot) startChatAction(ctx context.Context, to replyTarget, action models.ChatAction) (stop func())
func (b *Bot) draftEligible(to replyTarget) bool
func (b *Bot) newDraftStream(parent context.Context, to replyTarget, draftID string, onFirst func()) *draftStream
func (b *Bot) newReplyOutput(turnCtx context.Context, turn *chatTurn, req turnRequest) replyOutput
func (b *Bot) bufferIntake(req turnRequest) // req.ctx and req.isEmojiOnly carry what the old args did
// /clear and /clear_hard share their argument parsing (today duplicated at handlers.go:409-428 and 463-482):
func parseClearArgs(command string, parts []string) (targetUserID, targetChatID int64, errReply string)
```
- Also changed: each of `sendStats`, `sendWhoAmI`, `sendRateLimitExceededMessage`, `clearChatHistory`, `handleStickerMessage`, `generateStickerResponse`, `handleVoiceMessage`, `handlePhotoMessage`, `uploadPhotoItems` and `bufferAlbumItem` replaces its `chatID int64` and `businessConnectionID string` parameters with a single `to replyTarget`, in the position of `chatID`. Their other parameters keep their order.
- `pendingAlbum` replaces its `chatID` and `businessConnectionID` fields with `to replyTarget`.
- `draftStream` replaces `chatID` with `to replyTarget`.

- [ ] **Step 1: Write the failing tests** in `topic_test.go`

```go
func TestTargetFor(t *testing.T)
// topic message {Chat.ID: -100500, IsTopicMessage: true, MessageThreadID: 77}, "" → {-100500, "", 77}
// reply thread in a non-forum supergroup {IsTopicMessage: false, MessageThreadID: 12}, "" → threadID 0
// private message, "biz-1" → businessConnectionID "biz-1", threadID 0
func TestSendResponse_TargetsTopic(t *testing.T)
// SendMessage params: ChatID == int64(-100500), MessageThreadID == 77, BusinessConnectionID == ""
func TestStartChatAction_TargetsTopic(t *testing.T)
// SendChatAction params: MessageThreadID == 77
func TestDraftStream_TargetsTopic(t *testing.T)
// newDraftStream(ctx, replyTarget{chatID: 123, threadID: 9}, "7", nil).update("Hi") → SendMessageDraft MessageThreadID == 9
func TestHandleUpdate_CommandInTopicRepliesInTopic(t *testing.T)
// /whoami from user 123 in supergroup -100500, IsTopicMessage true, MessageThreadID 77,
// Entities [{Type: "bot_command", Offset: 0, Length: 7}] → every SendMessage has MessageThreadID 77
func TestHandleUpdate_MessageOutsideTopicSendsNoThreadID(t *testing.T)
// same, IsTopicMessage false, MessageThreadID 12 → every SendMessage has MessageThreadID 0
func TestHandleEditedMessage_TopicKeepsThreadID(t *testing.T) // Review Focus 4
// Store user message (telegram id 77) in chat -100500 via storeUserMessage; register an uncommitted
// running turn covering it (beginTextTurn); handleUpdate with EditedMessage{ID: 77, IsTopicMessage: true,
// MessageThreadID: 41, Text: "fixed"} → b.turns[-100500].rerun.threadID == 41
// handlers_test.go
func TestParseClearArgs(t *testing.T)
// ("/clear", ["/clear"]) → 0, 0, ""
// ("/clear", ["/clear", "42"]) → 42, 0, ""
// ("/clear", ["/clear", "42", "-100"]) → 42, -100, ""
// ("/clear", ["/clear", "x"]) → errReply "Invalid user ID format. Usage: /clear [user_id] [chat_id]"
// ("/clear_hard", ["/clear_hard", "42", "y"]) → errReply "Invalid chat ID format. Usage: /clear_hard [user_id] [chat_id]"
```

- [ ] **Step 2: Run them:** `go test -run 'TestTargetFor|TargetsTopic|InTopic|OutsideTopic|TopicKeepsThreadID|TestParseClearArgs' ./...`. Expected: compile failure, because `replyTarget` and `parseClearArgs` are undefined.
- [ ] **Step 3: Add `replyTarget`, `targetFor` and `turnRequest.threadID`/`target()`.** `targetFor` uses `msg.Chat.ID` and sets `threadID = msg.MessageThreadID` only when `msg.IsTopicMessage`.
- [ ] **Step 4: Change every send path to take `replyTarget`.**
  - Every `bot.SendMessageParams`, `bot.SendChatActionParams`, `bot.SendMessageDraftParams` and `bot.SendAudioParams` the bot builds gets `MessageThreadID: to.threadID`.
  - `handleUpdate` computes `to := targetFor(message, businessConnectionID)` once and passes it everywhere.
  - Every `turnRequest` gets its `threadID` from the message that produced it: `handleUpdate`, `handlePhotoMessage`, `handleEditedMessage` (via `targetFor(edited, edited.BusinessConnectionID)`) and the intake/album buffers, which keep the latest message's values as they do today.
  - `runTurn` and `deliverStoppedTurn` use `req.target()`.
  - Both `/clear` cases call `parseClearArgs`. On a non-empty `errReply` they log it with the user ID and send it, so the reply texts are unchanged.
- [ ] **Step 5: Update existing call sites in tests** to the new signatures (`replyTarget{chatID: 42, businessConnectionID: "biz-7"}` and so on). `bufferText` builds a `turnRequest`.
- [ ] **Step 6: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS, new tests included.
- [ ] **Step 7: README.** Under **Commands**, add one sentence: in forum groups the bot replies in the topic the message was sent in.
- [ ] **Step 8: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: the gate exits 0 and the token pass shows 1 clone instead of the baseline's 3. The `/clear` pair counted as two clone entries, and only the `config.go` pair remains.
- [ ] **Step 9: Commit**

```bash
git add telegram_helpers.go bot.go handlers.go typing.go draft.go turn.go intake_buffer.go album_buffer.go README.md topic_test.go typing_test.go draft_test.go intake_buffer_test.go handlers_test.go edit_stop_test.go
git commit -m "fix: reply in the forum topic a message came from"
```

---

### Task 3: `thinking_display: "updates"` switch and request header

**Files:**
- Modify: `config.go`, `anthropic.go`
- Test: `config_test.go`, `anthropic_test.go`

**Interfaces:**
- Produces:
```go
const ThinkingDisplayUpdates = "updates"
func (c *BotConfig) NotesEnabled() bool // ThinkingDisplay == ThinkingDisplayUpdates
func (b *Bot) buildMessageParams(messages []anthropic.BetaMessageParam, isEmojiOnly bool,
	username, firstName, lastName string, isPremium bool, languageCode string, messageTime int) anthropic.BetaMessageNewParams
```

- [ ] **Step 1: Write the failing tests**

```go
// config_test.go, TestThinkingConfig: add cases
{"adaptive updates", ThinkingModeAdaptive, ThinkingDisplayUpdates, ""},
{"updates without thinking", "", ThinkingDisplayUpdates, "'thinking_display' requires"},
{"updates with disabled", ThinkingModeDisabled, ThinkingDisplayUpdates, "'thinking_display' requires"},
// anthropic_test.go, TestThinkingParamFromConfig: add case
{"adaptive updates", ThinkingModeAdaptive, ThinkingDisplayUpdates, true, map[string]any{"type": "adaptive", "display": "updates"}},
// anthropic_test.go, new
func TestBuildMessageParams_NotesBeta(t *testing.T) //NOSONAR ...
// Thinking adaptive + ThinkingDisplay updates → params.Betas == [AnthropicBetaThinkingDisplayUpdates2026_08_18]
// same plus one MCP server → Betas contains both that and AnthropicBetaMCPClient2025_11_20
// ThinkingDisplay "" → len(params.Betas) == 0, and the marshaled "thinking" object has no "display" key
```

- [ ] **Step 2: Run them:** `go test -run 'TestThinkingConfig|TestThinkingParamFromConfig|TestBuildMessageParams' ./...`. Expected: FAIL, because `ThinkingDisplayUpdates` is undefined.
- [ ] **Step 3: Implement**
  - `validateConfig` accepts `ThinkingDisplayUpdates` under the same "requires adaptive" rule. The error for unknown values lists all three.
  - Move the parameter construction in `getAnthropicResponse` (through `params.Tools`) into `buildMessageParams`, unchanged, and append the notes beta when `b.config.NotesEnabled()`.
  - `logConfigAdvisories` logs, when notes are on: `[%s] thinking_display=updates: progress notes appear only on models that write them (Opus 5.5, Sonnet 5.5, Fable 5.x); system_prompts can ask for them`.
- [ ] **Step 4: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: the gate exits 0 and the semantic report shows nothing new.
- [ ] **Step 6: Commit**

```bash
git add config.go anthropic.go config_test.go anthropic_test.go
git commit -m "feat: accept thinking_display \"updates\" and send its beta header"
```

---

### Task 4: Status line controller and Business rights

**Files:**
- Modify: `telegram_client.go`, `telegram_client_mock.go`, `bot.go` (Bot fields and their `NewBot` initialization)
- Create: `status.go`, `status_test.go`

**Interfaces:**
- Consumes: `replyTarget` (Task 2), `clampRunes` and `telegramMessageLimit` (draft.go).
- Produces:
```go
// TelegramClient additions (signatures match *bot.Bot):
EditMessageText(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error)
DeleteMessage(ctx context.Context, params *bot.DeleteMessageParams) (bool, error)
DeleteBusinessMessages(ctx context.Context, params *bot.DeleteBusinessMessagesParams) (bool, error)
GetBusinessConnection(ctx context.Context, params *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error)
// MockTelegramClient: EditMessageTextFunc, DeleteMessageFunc, DeleteBusinessMessagesFunc, GetBusinessConnectionFunc.
// Defaults when nil: (&models.Message{}, nil), (true, nil), (true, nil), and
// (&models.BusinessConnection{IsEnabled: true, Rights: &models.BusinessBotRights{CanReply: true, CanDeleteSentMessages: true}}, nil).

// Bot fields: statusFirstDelay, statusEditInterval time.Duration (NewBot sets the defaults);
// businessRights map[string]businessRightsEntry; businessRightsMu sync.Mutex.
const (
	defaultStatusFirstDelay   = 1 * time.Second
	defaultStatusEditInterval = 1 * time.Second
	businessRightsTTL         = 10 * time.Minute
)
func (b *Bot) newStatusLine(turnCtx, deliverCtx context.Context, to replyTarget) *statusLine
func (s *statusLine) start()
func (s *statusLine) note(text string) // non-blocking; keeps only the newest
func (s *statusLine) answerSent()      // segment mode: drop the waiting note, delete the shown message
func (s *statusLine) freeze()          // draft mode: drop the waiting note, keep the message, ignore later notes
func (s *statusLine) close()           // stop, wait for any call in flight, delete the message; idempotent
func (b *Bot) businessNotesAllowed(ctx context.Context, connectionID string) bool
```

Behavior (from the spec):
- **One sender goroutine:** like `draftStream`, one goroutine owns every Telegram call, using `turnCtx`.
- **First send:** happens once the earliest waiting note is `statusFirstDelay` old, and shows the newest note: `SendMessage{ChatID, MessageThreadID, BusinessConnectionID, Text, DisableNotification: true}`.
- **Edits:** later notes edit (`EditMessageText{ChatID, MessageID, BusinessConnectionID, Text}`) no sooner than `statusEditInterval` after the previous send or edit. Unchanged text is skipped. An error whose text contains `message is not modified` counts as success.
- **Failures:** any other send or edit error ends notes for the turn, but an existing message is still deleted on `close`.
- **Business check:** before the turn's first send, when `to.businessConnectionID != ""` and `!businessNotesAllowed(...)`, notes end for the turn. Log it once.
- **Deletes:** Business messages use `DeleteBusinessMessages{BusinessConnectionID, MessageIDs: []int{id}}`; everything else uses `DeleteMessage{ChatID, MessageID}`. A failed delete is logged.
- **`close`:** deletes with `deliverCtx`, so it still works after a restart has cancelled `turnCtx`.
- **`businessNotesAllowed`:** true when `conn.IsEnabled && conn.Rights != nil && conn.Rights.CanReply && conn.Rights.CanDeleteSentMessages`.

- [ ] **Step 1: Write the failing tests** in `status_test.go`.
  - Tests set `b.statusFirstDelay` and `b.statusEditInterval` to 20 ms.
  - A recorder captures every Telegram call in order as `{kind, params}`, and `SendMessage` returns IDs 501, 502, and so on.
  - The target is `{chatID: -100500, threadID: 77}` unless noted.

```go
func TestStatusLine_NoNotesMakesNoCalls(t *testing.T)               // start, close → no calls
func TestStatusLine_FirstNoteWaitsThenSendsSilently(t *testing.T)    // note("Checking the docs") → no call before 10 ms; then one send: Text "Checking the docs", DisableNotification true, ChatID -100500, MessageThreadID 77
func TestStatusLine_FirstSendShowsNewestNote(t *testing.T)           // note("A"), note("B") within the first delay → the send has Text "B"
func TestStatusLine_LaterNotesEditInPlaceThrottled(t *testing.T)     // after send 501: note("C"), note("D") → exactly one edit, MessageID 501, Text "D", ≥ edit interval after the send
func TestStatusLine_UnchangedNoteNotEdited(t *testing.T)             // send "A"; note("A") → no edit
func TestStatusLine_NotModifiedErrorIsHarmless(t *testing.T)         // edit returns errors.New("Bad Request: message is not modified"); note("E") → a further edit with "E"
func TestStatusLine_AnswerSentDeletesThenNewNoteStartsNew(t *testing.T) // send 501; answerSent → DeleteMessage{-100500, 501}; note("F") → new send 502
func TestStatusLine_AnswerSentDropsWaitingNote(t *testing.T)         // note("A"); answerSent before the first delay → no call within 5× first delay
func TestStatusLine_FreezeKeepsMessageAndIgnoresNotes(t *testing.T)  // send 501; freeze; note("G") → no edit; close → DeleteMessage 501
func TestStatusLine_CloseDeletesWithDeliverCtx(t *testing.T)         // send 501; cancel turnCtx; close → DeleteMessage 501 recorded when close returns
func TestStatusLine_SendFailureStopsNotes(t *testing.T)              // send errors; note("H") → no further calls; close → no delete
func TestStatusLine_ClampsLongNote(t *testing.T)                     // 5000-rune note → sent Text has 4096 runes
func TestStatusLine_BusinessUsesBusinessDelete(t *testing.T)         // to {chatID 555, businessConnectionID "biz-1"}: rights lookup, then send with BusinessConnectionID "biz-1"; close → DeleteBusinessMessages{"biz-1", [501]}, no DeleteMessage
func TestStatusLine_BusinessWithoutDeleteRightShowsNothing(t *testing.T) // CanDeleteSentMessages false → only the rights lookup; no send
func TestStatusLine_BusinessDisabledConnectionShowsNothing(t *testing.T) // Review Focus 5: IsEnabled false, full rights → no send
func TestStatusLine_BusinessLookupFailureShowsNothing(t *testing.T)  // GetBusinessConnection returns error → no send
func TestBusinessNotesAllowed_CachesSuccessForTTL(t *testing.T)      // two calls → one lookup; MockClock.Advance(11*time.Minute) → second lookup; two failing calls → two lookups
func TestStatusLine_NoteAfterCloseIsIgnored(t *testing.T)            // Review Focus 1: close; note("late") → no call within 5× first delay
func TestStatusLine_InFlightSendDeletedOnClose(t *testing.T)         // Review Focus 2: SendMessage blocks 60 ms then returns ID 900; close() during it → when close returns, DeleteMessage 900 recorded
```

- [ ] **Step 2: Run them:** `go test -run 'TestStatusLine|TestBusinessNotesAllowed' ./...`. Expected: compile failure, because `newStatusLine` is undefined.
- [ ] **Step 3: Add the four client methods and mock functions, then implement `status.go`** following the behavior list above.
- [ ] **Step 4: Run them with the race detector:** `go test -race -count=3 -run 'TestStatusLine|TestBusinessNotesAllowed' ./...`. Expected: PASS three times.
- [ ] **Step 5: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 6: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: the gate exits 0. `statusLine` and `draftStream` are both single-sender loops, so any semantic pair between them must either share a helper or be explained in the task report.
- [ ] **Step 7: Commit**

```bash
git add telegram_client.go telegram_client_mock.go bot.go status.go status_test.go
git commit -m "feat: status line that shows, edits and removes one progress message"
```

---

### Task 5: Reply assembler: notes vs answer text in the stream

**Files:**
- Create: `reply_assembler.go`, `reply_assembler_test.go`, `anthropic_stream_test.go`
- Modify: `anthropic.go` (`getAnthropicResponse`, `streamMessages`), `handlers.go` (the four `getAnthropicResponse` callers)

**Interfaces:**
- Consumes: `joinNonEmpty` (anthropic.go); `buildMessageParams` and `NotesEnabled` (Task 3).
- Produces:
```go
type replySinks struct {
	onSegment  func(string) error // an answer text block is final
	onProgress func(string)       // the answer so far, for in-place redraws
	onNote     func(string)       // a progress note; nil turns notes off for the turn
}
const maxTextNoteRunes = 280
const interruptedNotePlaceholder = "This part of the response was interrupted before it finished."
func newReplyAssembler(s replySinks) *replyAssembler
func (a *replyAssembler) blockStart(kind string)
func (a *replyAssembler) progress(currentText string) // onProgress(joinNonEmpty(answer+held so far, currentText))
func (a *replyAssembler) textDone(text string)
func (a *replyAssembler) thinkingDone(text string)
func (a *replyAssembler) responseEnd(stop anthropic.BetaStopReason)
func (a *replyAssembler) reply() string
// changed:
func (b *Bot) getAnthropicResponse(ctx context.Context, chatID int64, messages []anthropic.BetaMessageParam,
	isEmojiOnly bool, username, firstName, lastName string, isPremium bool, languageCode string,
	messageTime int, sinks replySinks) (string, error)
func (b *Bot) streamMessages(ctx context.Context, params anthropic.BetaMessageNewParams, a *replyAssembler) (anthropic.BetaMessage, error)
```

Rules:
- **Notes off** (`onNote == nil`) behaves exactly like today.
  - `textDone` trims the text. Non-empty text is committed at once: appended to `reply` with `joinNonEmpty`, then `onSegment` is called. An `onSegment` error is logged as `[stream] onSegment failed`.
  - `thinkingDone`, `blockStart` and `responseEnd` do nothing.
- **Notes on:**
  - `thinkingDone`: trimmed text that is non-empty and not the placeholder goes to `onNote`, logged as `[note] …` (truncated to 500 characters).
  - `textDone`: empty text and the placeholder are dropped. Text over 280 runes is committed at once. Anything else is held.
  - `blockStart(kind)` with held text: for `tool_use`, `server_tool_use` or `mcp_tool_use`, the held text goes to `onNote`, then `onProgress(reply)` runs so the draft drops it. For any other kind, the held text is committed.
  - `responseEnd`: `pause_turn` keeps the held text; anything else commits it.
- **`getAnthropicResponse`** builds one assembler for the whole turn, shared across `pause_turn` continuations and file self-heal retries, calls `responseEnd` after each stream, and returns `a.reply()`. An empty reply still produces `emptyStreamError`.

- [ ] **Step 1: Write the failing tests.** Sinks record segments, notes and progress calls.

```go
// reply_assembler_test.go
func TestReplyAssembler_NotesOffMatchesToday(t *testing.T)        // textDone(" Hi ") → segments ["Hi"]; thinkingDone("Checking") → no note; blockStart("mcp_tool_use"); textDone("Short.") → segments ["Hi","Short."] at once; reply "Hi\n\nShort."
func TestReplyAssembler_ThinkingNote(t *testing.T)                // notes on: thinkingDone(" Checking the docs ") → notes ["Checking the docs"]; reply ""
func TestReplyAssembler_EmptyThinkingIgnored(t *testing.T)        // thinkingDone("  ") → no note
func TestReplyAssembler_PlaceholderNeverShown(t *testing.T)       // thinkingDone(placeholder); textDone(placeholder); responseEnd(end_turn) → no notes, no segments
func TestReplyAssembler_ShortTextBeforeToolIsNote(t *testing.T)   // table "tool_use","server_tool_use","mcp_tool_use": textDone("Let me check the docs.") → no segment; blockStart(kind) → notes ["Let me check the docs."], reply "", last progress ""
func TestReplyAssembler_ShortTextBeforeOtherBlockIsAnswer(t *testing.T) // table "text","thinking": textDone("Sure."); blockStart(kind) → segments ["Sure."], no notes
func TestReplyAssembler_LongTextBeforeToolIsAnswer(t *testing.T)  // 281-rune text → segment right after textDone; blockStart("mcp_tool_use") → no note
func TestReplyAssembler_RuneBoundary(t *testing.T)                // Review Focus 3: 280 × "ж" (560 bytes) then blockStart("tool_use") → note; 281 × "ж" → segment
func TestReplyAssembler_HeldTextFlushedAtEnd(t *testing.T)        // textDone("All done."); responseEnd(end_turn) → segments ["All done."]
func TestReplyAssembler_PauseTurnKeepsHeldText(t *testing.T)      // textDone("Let me look."); responseEnd(pause_turn) → nothing; blockStart("server_tool_use") → notes ["Let me look."]
// anthropic_stream_test.go
func TestGetAnthropicResponse_StreamNotesAndAnswer(t *testing.T)
// An httptest server answers POST /v1/messages with an SSE stream:
//   message_start
//   thinking block 0: thinking_delta "Searching the docs for the candle farm.", then signature_delta
//   mcp_tool_use block 1: name "list_documents", server_name "tibik-docs", input_json_delta {"query":"candle farm"}
//   mcp_tool_result block 2
//   text block 3: text_delta "Open the Farm tab."
//   message_delta (stop_reason end_turn), then message_stop
// b.anthropicClient = anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"))
// notes on → notes ["Searching the docs for the candle farm."], segments ["Open the Farm tab."], reply "Open the Farm tab.",
//   and the request's anthropic-beta header contains "thinking-display-updates-2026-08-18"
func TestGetAnthropicResponse_StreamNotesOff(t *testing.T)         // same stream, notes off → no notes, segments ["Open the Farm tab."], no notes beta in the header
```

- [ ] **Step 2: Run them:** `go test -run 'TestReplyAssembler|TestGetAnthropicResponse_Stream' ./...`. Expected: compile failure, because `newReplyAssembler` is undefined.
- [ ] **Step 3: Implement `reply_assembler.go`.** Then rework `streamMessages` to report block starts, text deltas, text stops and thinking stops to the assembler; the existing logging stays as it is. Change `getAnthropicResponse` to take `sinks`.
  - `respondToChat` and `handlePhotoMessage` pass `replySinks{onSegment: out.onSegment, onProgress: out.onProgress}`.
  - The voice and sticker paths pass `replySinks{}`.
- [ ] **Step 4: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: the gate exits 0 and the semantic report shows nothing new.
- [ ] **Step 6: Commit**

```bash
git add reply_assembler.go reply_assembler_test.go anthropic_stream_test.go anthropic.go handlers.go
git commit -m "feat: tell progress notes apart from answer text in the stream"
```

---

### Task 6: Wire notes into turns, voice, and the README

**Files:**
- Modify: `draft.go` (`replyOutput`, `newReplyOutput`), `turn.go` (`runTurn`, `deliverStoppedTurn`), `handlers.go` (`respondToChat`, `handlePhotoMessage`, `handleVoiceMessage`), `README.md`
- Create: `status_turn_test.go`

**Interfaces:**
- Consumes: `statusLine` (Task 4); `replySinks` and the new `getAnthropicResponse` (Task 5); `newReplyOutput(turnCtx, turn, req)` (Task 2).
- Produces:
```go
type replyOutput struct {
	onSegment  func(string) error
	onProgress func(string)
	onNote     func(string) // nil when notes are off
	finish     func(ctx context.Context, joined string) error
	partial    func() string
	dropStatus func()       // delete the status message now and end notes; idempotent; no-op when off
	close      func()
}
func (o replyOutput) sinks() replySinks
```

Wiring:
- **Notes off:** `newReplyOutput` is unchanged and `onNote` is nil.
- **Notes on:** it creates `b.newStatusLine(turnCtx, req.ctx, req.target())`, starts it, and sets `onNote = status.note` and `dropStatus = status.close`.
  - **Segment mode:** `onSegment` sends the segment as today (it still calls `markCommitted`), then calls `status.answerSent()`.
  - **Draft mode:** `onSegment` only calls `status.freeze()`. `finish` closes the draft, sends the final message, then calls `status.close()`.
  - **Both:** `close` also calls `status.close()`.
- **`runTurn`:** passes `out.sinks()`. On an error it calls `out.dropStatus()` before sending the error reply, and `deliverStoppedTurn` calls it before sending the partial. On a restart, the deferred `out.close()` removes the status message.
- **`handleVoiceMessage`, notes on:** creates a status line with `turnCtx` and the handler's original `ctx` (captured before `ctx = turnCtx`) and defers its `close`, which runs after the audio or text fallback is sent. It passes `replySinks{onNote: status.note}`. On a model error it closes the status line before sending the error reply.
- **Sticker replies:** keep `replySinks{}`.

- [ ] **Step 1: Write the failing tests** in `status_turn_test.go`.
  - Tests use `setupBotForTest`, notes on (`Thinking` adaptive, `ThinkingDisplay` updates), and 20 ms status timings.
  - A recorder captures sends, edits and deletes in order. Status sends return IDs 501, 502 and so on.
  - Each test calls `runTurn` directly with a scripted `call`, so no Anthropic request is made.

```go
func TestRunTurn_NotesOffMakesTodaysCalls(t *testing.T)            // notes off: call asserts out.onNote == nil, calls out.onSegment("Here you go") → one SendMessage "Here you go", no edits/deletes/rights lookups
func TestRunTurn_SegmentModeStatusThenAnswerThenDelete(t *testing.T) // call: onNote("Checking the docs"); wait for status send; onSegment("Here you go"); return it → order: send "Checking the docs" (silent), send "Here you go" (DisableNotification false), DeleteMessage 501; last assistant message in memory "Here you go"
func TestRunTurn_SegmentModeInterleaving(t *testing.T)             // note A → sent 501 → segment "Part one" → note B → sent 503 → segment "Part two" → order: send A, send "Part one", delete 501, send B, send "Part two", delete 503
func TestRunTurn_DraftModeDeletesStatusAfterFinalMessage(t *testing.T) // StreamDrafts on, chat 123: note A → sent 501; onProgress("Here"); onSegment("Here you go"); onNote("late") → no edit with "late"; final SendMessage "Here you go" recorded before DeleteMessage 501
func TestRunTurn_ErrorDeletesStatusBeforeErrorReply(t *testing.T)  // note → sent 501; return "", errors.New("boom") → DeleteMessage 501 recorded before the error reply SendMessage
func TestRunTurn_RestartDeletesStatus(t *testing.T)                // turn from beginTextTurn(req, 5); call: note → sent 501 → <-ctx.Done() → return "", ctx.Err(); the test restarts via beginTextTurn(newer, 7), which only works because a shown note does not commit the turn, then b.discardRerun(chat) so no rerun calls the API → DeleteMessage 501; no error reply
func TestRunTurn_StopDeletesStatusBeforePartial(t *testing.T)      // StreamDrafts on, chat 123: note → sent 501; onProgress("Partial answer") → wait for the draft send; b.stopTurn(123, <draft id>); call returns ctx.Err() → DeleteMessage 501 recorded before SendMessage "Partial answer"
```

- [ ] **Step 2: Run them:** `go test -run 'TestRunTurn_' ./...`. Expected: FAIL. Notes are not wired yet, so there are no status sends or deletes.
- [ ] **Step 3: Implement the wiring above.**
- [ ] **Step 4: Run the full suite:** `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5: Update the README.**
  - In the `thinking_display` row, list `"updates"` and link to the new section.
  - Add a **Progress notes** section after "Draft streaming and the Stop button". It covers:
    - which models write notes (Opus 5.5, Sonnet 5.5, Fable 5.x) and that others accept the setting and write none;
    - that the status message is sent silently, edited in place, and deleted once the answer is sent;
    - drafts: notes stop once the answer streams;
    - Business: the bot needs the "delete sent messages" right, otherwise that chat shows no notes;
    - the suggested `custom_instructions` line: "Before you call a tool, write one short line in the user's language saying what you're about to check.";
    - that notes are billed as output tokens and logged as `[note]`.
- [ ] **Step 6: Duplication check:** run the gate and the semantic report from Global Constraints. Expected: the gate exits 0. The voice path and `newReplyOutput` both start a status line, so a semantic pair between them must either share a helper or be explained.
- [ ] **Step 7: Commit**

```bash
git add draft.go turn.go handlers.go status_turn_test.go README.md
git commit -m "feat: show progress notes during text, photo and voice turns"
```

---

### Task 7: Full verification and live check

- [ ] **Step 1: Run the checks:** `go build ./... && go vet ./... && go test -race -count=1 ./...`. Expected: all pass.
- [ ] **Step 2: Run the linters CI uses:** `go install github.com/securego/gosec/v2/cmd/gosec@v2.30.0 && gosec ./...`, then `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 && golangci-lint run --timeout 5m`. Expected: no issues. Fix any findings in the task's own files and commit them as `fix: address lint findings`.
- [ ] **Step 3: Final duplication report** against the starting commit `401b263`:
  - Gate: `jscpd --format go --ignore "**/*_test.go" --similarity --baseline-from-ref 401b263 --fail-on-new-clones -r console .` must exit 0.
  - Semantic report on the whole branch: the command from Global Constraints.
  - For information only: `jscpd --format go -r console .`, which includes tests.
  - Expected: no new clones against `401b263`, production duplication below the 0.74% baseline, and no semantic pairs. Put all three outputs in the final report.
- [ ] **Step 4: Live check (owner, after deploying).**
  - Set `"thinking_display": "updates"` in tibik_bot's config and add the suggested line to `custom_instructions`.
  - Ask a docs question in a private chat, a group, a forum topic and a Business chat, and send one voice note.
  - Expected:
    - any note appears silently and then disappears after the answer;
    - the answer notifies;
    - topic replies stay in the topic;
    - a Business connection without the delete right shows no notes;
    - `[note]` lines appear in the log.

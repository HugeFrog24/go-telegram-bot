package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notesGroupChat is a group: never eligible for drafts, so its turns always
// send segments.
const notesGroupChat int64 = -100600

// turnCall is one Telegram message call a turn made, in the order made.
type turnCall struct {
	kind   string // send, edit, delete, deleteBusiness, rights
	text   string // send and edit
	id     int    // send: the ID it returned; edit and delete: the message targeted
	silent bool   // send: DisableNotification
}

// turnTelegram stands in for Telegram during a whole turn. It records sends,
// edits, deletes and rights lookups in one ordered list, and drafts apart
// from them. Sends get IDs 501, 502 and so on, handed out as each call
// begins, so IDs follow the recorded order even while a send is in flight.
type turnTelegram struct {
	mu     sync.Mutex
	calls  []turnCall
	drafts []string
	nextID int
}

func (r *turnTelegram) record(c turnCall) turnCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.kind == "send" {
		c.id = r.nextID
		r.nextID++
	}
	r.calls = append(r.calls, c)
	return c
}

func (r *turnTelegram) install(m *MockTelegramClient) {
	m.SendMessageFunc = func(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
		c := r.record(turnCall{kind: "send", text: p.Text, silent: p.DisableNotification})
		return &models.Message{ID: c.id}, nil
	}
	m.EditMessageTextFunc = func(_ context.Context, p *bot.EditMessageTextParams) (*models.Message, error) {
		r.record(turnCall{kind: "edit", text: p.Text, id: p.MessageID})
		return &models.Message{ID: p.MessageID}, nil
	}
	m.DeleteMessageFunc = func(_ context.Context, p *bot.DeleteMessageParams) (bool, error) {
		r.record(turnCall{kind: "delete", id: p.MessageID})
		return true, nil
	}
	m.DeleteBusinessMessagesFunc = func(_ context.Context, p *bot.DeleteBusinessMessagesParams) (bool, error) {
		for _, id := range p.MessageIDs {
			r.record(turnCall{kind: "deleteBusiness", id: id})
		}
		return true, nil
	}
	m.GetBusinessConnectionFunc = func(context.Context, *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error) {
		r.record(turnCall{kind: "rights"})
		return &models.BusinessConnection{IsEnabled: true, Rights: fullBusinessRights()}, nil
	}
	m.SendMessageDraftFunc = func(_ context.Context, p *bot.SendMessageDraftParams) (bool, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.drafts = append(r.drafts, p.Text)
		return true, nil
	}
}

func (r *turnTelegram) snapshot() []turnCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]turnCall(nil), r.calls...)
}

func (r *turnTelegram) draftTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.drafts...)
}

// waitSend waits until a message with text has been sent. The timeout only
// matters when a test fails, so it is generous for loaded runners.
func (r *turnTelegram) waitSend(t *testing.T, text string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, c := range r.snapshot() {
			if c.kind == "send" && c.text == text {
				return true
			}
		}
		return false
	}, 5*time.Second, 2*time.Millisecond, "expected a send of %q", text)
}

// waitDraftShown waits until the turn's draft has reached Telegram. The draft
// marks the turn committed only after it has recorded the text as shown, so
// from here on the turn's partial is that text.
func waitDraftShown(t *testing.T, b *Bot, turn *chatTurn) {
	t.Helper()
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		return turn.committed
	}, 5*time.Second, 2*time.Millisecond, "expected the draft to reach Telegram")
}

// setupNotesTurn builds a bot with progress notes on or off, 20 ms status
// timings and a recorder in place of Telegram.
func setupNotesTurn(t *testing.T, notes, drafts bool) (*Bot, *turnTelegram) {
	t.Helper()
	b, mockTg := setupBotForTest(t, 123)
	if notes {
		b.config.Thinking = ThinkingModeAdaptive
		b.config.ThinkingDisplay = ThinkingDisplayUpdates
	}
	b.config.StreamDrafts = drafts
	b.statusFirstDelay = testStatusTiming
	b.statusEditInterval = testStatusTiming
	rec := &turnTelegram{nextID: 501}
	rec.install(mockTg)
	return b, rec
}

func startNotesTurn(t *testing.T, b *Bot, req turnRequest, wantID uint64) (*chatTurn, context.Context) {
	t.Helper()
	turn, turnCtx, ok := b.beginTextTurn(req, wantID)
	require.True(t, ok)
	return turn, turnCtx
}

func TestRunTurn_NotesOffMakesTodaysCalls(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	t.Run("segments", func(t *testing.T) {
		b, rec := setupNotesTurn(t, false, false)
		req := textReq(notesGroupChat)
		req.businessConnectionID = "biz-notes-off"
		turn, turnCtx := startNotesTurn(t, b, req, 1)

		b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
			assert.Nil(t, out.onNote, "notes off: no note sink")
			assert.Nil(t, out.sinks().onNote)
			return "Here you go", out.onSegment("Here you go")
		})

		assert.Equal(t, []turnCall{{kind: "send", text: "Here you go", id: 501}}, rec.snapshot(),
			"one answer message; no status message, edit, delete or rights lookup")
	})

	t.Run("drafts", func(t *testing.T) {
		b, rec := setupNotesTurn(t, false, true)
		req := textReq(123)
		turn, turnCtx := startNotesTurn(t, b, req, 1)

		b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
			assert.Nil(t, out.onNote, "notes off: no note sink")
			assert.Nil(t, out.onSegment, "notes off: the draft takes no segments, as today")
			out.onProgress("Here you go")
			return "Here you go", nil
		})

		assert.Equal(t, []turnCall{{kind: "send", text: "Here you go", id: 501}}, rec.snapshot())
	})
}

func TestRunTurn_SegmentModeStatusThenAnswerThenDelete(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, false)
	req := textReq(notesGroupChat)
	turn, turnCtx := startNotesTurn(t, b, req, 1)

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")
		return "Here you go", sinks.onSegment("Here you go")
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "send", text: "Here you go", id: 502},
		{kind: "delete", id: 501},
	}, rec.snapshot(), "the status message is silent, the answer notifies, and the delete comes last")

	msgs := b.getOrCreateChatMemory(chatKey(notesGroupChat)).Messages
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	assert.False(t, last.IsUser)
	assert.Equal(t, "Here you go", last.Text, "memory holds the answer, never the note")
}

func TestRunTurn_SegmentModeInterleaving(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, false)
	req := textReq(notesGroupChat)
	turn, turnCtx := startNotesTurn(t, b, req, 1)

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")
		require.NoError(t, sinks.onSegment("Part one"))
		sinks.onNote("Checking the changelog")
		rec.waitSend(t, "Checking the changelog")
		require.NoError(t, sinks.onSegment("Part two"))
		return "Part one\n\nPart two", nil
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "send", text: "Part one", id: 502},
		{kind: "delete", id: 501},
		{kind: "send", text: "Checking the changelog", id: 503, silent: true},
		{kind: "send", text: "Part two", id: 504},
		{kind: "delete", id: 503},
	}, rec.snapshot(), "each segment removes the status message above it; the next note starts a new one below")
}

func TestRunTurn_DraftModeDeletesStatusAfterFinalMessage(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, true)
	req := textReq(123)
	turn, turnCtx := startNotesTurn(t, b, req, 1)

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		require.NotNil(t, sinks.onSegment, "notes on: draft mode learns when answer text is final")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")
		sinks.onProgress("Here")
		require.NoError(t, sinks.onSegment("Here you go"))
		// Answer text is in the draft: a status edit now would clear it.
		sinks.onNote("late")
		// Frozen, the status line drops the note at once, so nothing can be
		// recorded here; unfrozen, the edit lands within one edit interval.
		assert.Never(t, func() bool { return len(rec.snapshot()) != 1 }, 5*testStatusTiming, 2*time.Millisecond,
			"a note after final answer text must make no edit or send")
		return "Here you go", nil
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "send", text: "Here you go", id: 502},
		{kind: "delete", id: 501},
	}, rec.snapshot(), "no edit for the late note; the final message goes out before the delete")
}

func TestStatusLine_DraftAnswerTextDropsWaitingNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, true)
	req := textReq(123)
	turn, turnCtx := startNotesTurn(t, b, req, 1)
	defer b.finishTurn(turn)

	// The output runTurn builds in draft mode, with the status line's sender
	// not started yet, so the note is certainly still waiting when answer
	// text reaches the draft.
	status := b.newStatusLine(turnCtx, req.ctx, req.target())
	out := b.newAnswerOutput(turnCtx, turn, req.target(), true).withStatus(status, true)
	defer out.close()
	sinks := out.sinks()
	sinks.onNote("Checking the docs")
	sinks.onProgress("Hel")
	status.start()

	assert.Never(t, func() bool { return len(rec.snapshot()) != 0 }, 5*testStatusTiming, 2*time.Millisecond,
		"answer text reached the draft within the first delay, so the note is never shown")
	require.NoError(t, out.finish(req.ctx, "Hello"))
	assert.Equal(t, []turnCall{{kind: "send", text: "Hello", id: 501}}, rec.snapshot(), "only the final message")
}

func TestRunTurn_DraftModeTextNoteEndToEnd(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const shortText, answer = "Let me search the web.", "The candle farm is on the Farm tab."
	b, rec := setupNotesTurn(t, true, true)
	b.draftInterval = testDraftInterval / 4
	b.draftKeepalive = testDraftInterval / 2
	var timers atomic.Int64
	b.draftAfter = countingAfter(&timers)

	// The model streams a short text, calls a tool, then answers. The stream
	// stops after the tool call until the test lets the search finish.
	events := evStream(
		evTextBlock(0, shortText),
		evServerToolBlock(1),
		evTextBlock(2, answer),
		evMessageEnd("end_turn"),
	)
	stub, finishSearch := newGatedAnthropicStub(t, events, 7) // message_start, then three events per block
	useAnthropicStub(b, stub)

	req := textReq(123)
	turn, turnCtx := startNotesTurn(t, b, req, 1)
	began := time.Now()
	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
			messages := []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("Where is the candle farm?")),
			}
			return b.getAnthropicResponse(ctx, req.conv(), messages, false, req.username, "", "", false, "en", 0, out.sinks())
		})
	}()
	waitTurn := func() {
		select {
		case <-turnDone:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn did not finish")
		}
	}
	t.Cleanup(func() { // runs before the stub's own cleanup
		finishSearch()
		waitTurn()
	})

	// The tool call made the short text a note: it is on screen as the
	// status message while the search runs.
	rec.waitSend(t, shortText)
	armed := timers.Load()
	paused := time.Now()
	time.Sleep(10 * b.draftKeepalive)
	armed = timers.Load() - armed
	// A sleeping sender arms a few timers here, at most about two per
	// keepalive even if the stream goroutine stalled before it redrew the
	// draft; a spinning one arms them by the hundred thousand.
	assert.LessOrEqual(t, armed, int64(4+2*time.Since(paused)/b.draftKeepalive),
		"the draft sender sleeps through the tool call")

	finishSearch()
	waitTurn()

	assert.Equal(t, []turnCall{
		{kind: "send", text: shortText, id: 501, silent: true},
		{kind: "send", text: answer, id: 502},
		{kind: "delete", id: 501},
	}, rec.snapshot(), "the note shows only in the status message, which goes once the final message is out")
	drafts := rec.draftTexts()
	assert.NotContains(t, drafts, "", "an empty draft is never sent")
	assert.Subset(t, []string{shortText, answer}, drafts, "the draft shows the streamed text and nothing else")
	// One send per new text, plus at most one keepalive per interval.
	assert.LessOrEqual(t, len(drafts), 3+int(time.Since(began)/b.draftKeepalive), "draft sends stay bounded")

	msgs := b.getOrCreateChatMemory(chatKey(123)).Messages
	require.NotEmpty(t, msgs)
	assert.Equal(t, answer, msgs[len(msgs)-1].Text, "memory holds the answer")
	for _, m := range msgs {
		assert.NotContains(t, m.Text, shortText, "the note is never stored")
	}
}

func TestRunTurn_ErrorDeletesStatusBeforeErrorReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, false)
	req := textReq(notesGroupChat)
	turn, turnCtx := startNotesTurn(t, b, req, 1)
	boom := errors.New("boom")

	b.runTurn(turn, turnCtx, req, func(_ context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")
		return "", boom
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "delete", id: 501},
		{kind: "send", text: b.anthropicErrorResponse(boom, req.userID), id: 502},
	}, rec.snapshot(), "the status message goes before the error reply")
}

func TestRunTurn_RestartDeletesStatus(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, false)
	req := textReq(notesGroupChat)
	turn, turnCtx := startNotesTurn(t, b, req, 5)

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")

		_, _, ok := b.beginTextTurn(textReq(notesGroupChat), 7)
		require.False(t, ok)
		require.Equal(t, cancelRestart, b.turnReason(turn), "a shown note must not commit the turn")
		// The rerun would call the model; this test is about the abandoned turn.
		dropRerun(b, notesGroupChat)

		<-ctx.Done()
		return "", ctx.Err()
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "delete", id: 501},
	}, rec.snapshot(), "the abandoned turn removes its status message and sends no error reply")
}

func TestRunTurn_StopDeletesStatusBeforePartial(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, true)
	req := textReq(123)
	turn, turnCtx := startNotesTurn(t, b, req, 1)

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		sinks.onNote("Checking the docs")
		rec.waitSend(t, "Checking the docs")
		sinks.onProgress("Partial answer")
		waitDraftShown(t, b, turn)

		require.True(t, b.stopTurn(chatKey(123), int(turn.seq)))
		<-ctx.Done()
		return "", ctx.Err()
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Checking the docs", id: 501, silent: true},
		{kind: "delete", id: 501},
		{kind: "send", text: "Partial answer", id: 502},
	}, rec.snapshot(), "the status message goes before the stopped partial")
}

func TestRunTurn_StopAfterTextNoteSendsNoPartial(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, rec := setupNotesTurn(t, true, true)
	req := textReq(123)
	turn, turnCtx := startNotesTurn(t, b, req, 1)

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		sinks := out.sinks()
		require.NotNil(t, sinks.onNote, "notes on: the note sink is wired")
		// A short text block streams into the draft, then a tool call makes it
		// a note: the assembler hands it over and redraws the draft without it.
		sinks.onProgress("Let me check")
		waitDraftShown(t, b, turn)
		sinks.onNote("Let me check")
		sinks.onProgress("")
		rec.waitSend(t, "Let me check")

		require.True(t, b.stopTurn(chatKey(123), int(turn.seq)))
		<-ctx.Done()
		return "", ctx.Err()
	})

	assert.Equal(t, []turnCall{
		{kind: "send", text: "Let me check", id: 501, silent: true},
		{kind: "delete", id: 501},
	}, rec.snapshot(), "the note was never answer text, so there is no partial to send")
	for _, m := range b.getOrCreateChatMemory(chatKey(123)).Messages {
		assert.True(t, m.IsUser, "nothing is stored for the bot, least of all the note: %q", m.Text)
	}
}

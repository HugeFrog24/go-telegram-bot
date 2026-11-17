package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// holdingStub answers every request with stream; the requests told to hold
// stop after split events (per request, splitAt) until open(i) is called.
type holdingStub struct {
	*anthropicStub
	mu     sync.Mutex
	gates  map[int]chan struct{}
	splits map[int]int
}

func newHoldingStub(t *testing.T, stream []string, split int, hold ...int) *holdingStub {
	t.Helper()
	return newHoldingStubOf(t, [][]string{stream}, split, hold...)
}

// newHoldingStubOf is newHoldingStub answering request i with streams[i],
// the last one for the rest.
func newHoldingStubOf(t *testing.T, streams [][]string, split int, hold ...int) *holdingStub {
	t.Helper()
	s := &holdingStub{anthropicStub: &anthropicStub{streams: streams}, gates: map[int]chan struct{}{}, splits: map[int]int{}}
	for _, i := range hold {
		s.gates[i] = make(chan struct{})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/files") {
			serveFilesStub(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.anthropicStub.mu.Lock()
		n := len(s.betas)
		s.betas = append(s.betas, r.Header.Get("anthropic-beta"))
		s.bodies = append(s.bodies, body)
		s.anthropicStub.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		s.mu.Lock()
		gate, at := s.gates[n], split
		if v, ok := s.splits[n]; ok {
			at = v
		}
		s.mu.Unlock()
		events := streams[min(n, len(streams)-1)]
		if gate != nil {
			_, _ = io.WriteString(w, sseFrames(events[:at]))
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			events = events[at:]
		}
		_, _ = io.WriteString(w, sseFrames(events))
	}))
	t.Cleanup(func() {
		for i := range s.gates {
			s.open(i)
		}
		srv.Close()
	})
	s.url = srv.URL
	return s
}

// splitAt makes held request i stop after n events instead.
func (s *holdingStub) splitAt(i, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.splits[i] = n
}

func (s *holdingStub) open(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.gates[i]; g != nil {
		select {
		case <-g:
		default:
			close(g)
		}
	}
}

func (s *holdingStub) waitRequests(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(s.requestBetas()) >= n }, 5*time.Second, time.Millisecond,
		"waiting for request %d", n)
}

var carol = &models.User{ID: 205, Username: "carol"}

// niceSticker is the model's reply to Mallory's sticker.
const niceSticker = "Nice sticker, mallory!"

// hookedSends records every message sent, and runs hook once, on the first
// send whose text is match, recording that send's context error after it.
type hookedSends struct {
	mu     sync.Mutex
	sent   []string
	once   sync.Once
	match  string
	hook   func()
	ctxErr error
	hooked atomic.Bool
}

func (s *hookedSends) install(tg *MockTelegramClient) {
	tg.SendMessageFunc = func(ctx context.Context, p *bot.SendMessageParams) (*models.Message, error) {
		if s.match != "" && p.Text == s.match {
			s.once.Do(func() {
				s.hook()
				s.ctxErr = ctx.Err()
				s.hooked.Store(true)
			})
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sent = append(s.sent, p.Text)
		return &models.Message{}, nil
	}
}

func (s *hookedSends) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// lastIndex is the index of the last s in texts, or -1.
func lastIndex(texts []string, s string) int {
	at := -1
	for i, text := range texts {
		if text == s {
			at = i
		}
	}
	return at
}

// Scoped r5 P1 inverted (K1): Mallory's sticker waits with ticket T while
// Alice's batch runs ahead; Carol's text restarts that turn, whose rerun
// answers Carol's too, so answeredThrough passes T before the sticker joins.
// Mallory then clears her messages while her sticker's turn calls the model:
// the turn is stopped though its message is below answeredThrough, and no
// reply to the cleared sticker is sent or filed.
func TestClear_OwnClearStopsTheirMediaTurnBelowAnsweredThrough(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStub(t, pinStream(), 1, 0, 2) // Alice's request and the sticker's wait
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sent := recordSends(tg)
	ctx := context.Background()
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)

	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 10, alice, "hi all"))
	stickerDone := make(chan struct{})
	go func() { defer close(stickerDone); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 11, mallory)) }()
	stub.waitRequests(t, 1) // Alice's batch ran ahead of the sticker; nothing shown yet
	waitForMediaWaiter(t, b, groupChatID)
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 12, carol, "what is new today?"))
	flushPending(t, b, groupChatID) // restarts Alice's turn: its rerun reads Carol's text
	stub.waitRequests(t, 3)         // the rerun answered, the sticker asked
	b.turnsMu.Lock()
	running := b.turns[chatKey(groupChatID)]
	b.turnsMu.Unlock()
	require.NotNil(t, running)

	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 13, mallory, "/clear"))
	assert.True(t, b.clearedTurn(running), "her sticker's turn is stopped")
	stub.open(2)
	<-stickerDone
	waitIdle(t, b, groupChatID)

	texts := sentTexts(sent)
	assert.Equal(t, len(texts)-1, lastIndex(texts, ownClearedNotice), "nothing after the confirmation")
	assert.Equal(t, 1, strings.Count(strings.Join(memoryTexts(b, groupChatID), "|"), "Noted,\n\nyour PIN is 9876."),
		"only the rerun's reply is filed, none to the cleared sticker")
}

// Blind r4 #2 inverted (K1): Mallory's sticker turn also covers Alice's
// unanswered message, and has shown nothing, when Mallory clears her
// messages: the turn is stopped, nothing answers the cleared sticker, and
// Alice's message runs as a text turn of its own.
func TestClear_OwnClearStopsTheirMediaTurnAndRerunsOthersText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStubOf(t, [][]string{
		evStream(evTextBlock(0, "Nice sticker, mallory!"), evMessageEnd("end_turn")),
		evStream(evTextBlock(0, "Hello, Alice!"), evMessageEnd("end_turn")),
	}, 1, 0)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	storeTextFrom(t, b, groupChatID, alice, "anyone here?") // unanswered, with no turn of its own
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, rv4Sticker(groupChatID, 900, mallory))
	}()
	stub.waitRequests(t, 1)

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 901, mallory, "/clear"))
	stub.open(0)
	<-done
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{ownClearedNotice, "Hello, Alice!"}, sentTexts(sent), "no reply to the cleared sticker")
	require.Len(t, stub.requestBetas(), 2, "Alice's message is answered by a text turn of its own")
	_, last := stub.lastMessage(t, 1)
	assert.Equal(t, "anyone here?", last)
	assert.False(t, requestHas(t, stub.anthropicStub, 1, "Sent a sticker"), "which does not read the sticker")
	assert.NotNil(t, storedRow(t, b, groupChatID, "anyone here?").AnsweredOn)
}

// A photo turn of Mallory's that has already shown part of its answer when
// she clears her messages goes on, but its reply is stored aside (G9).
func TestClear_OwnClearKeepsAShownMediaReplyAside(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub, open := newGatedAnthropicStub(t, pinStream(), 4) // the first segment shows
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	serveTelegramFiles(t, tg, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 910, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: mallory,
			Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}})
	}()
	require.Eventually(t, func() bool { return lastIndex(sentTexts(sent), "Noted,") >= 0 }, 5*time.Second, time.Millisecond)

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 911, mallory, "/clear"))
	open()
	<-done
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{"Noted,", ownClearedNotice, "your PIN is 9876."}, sentTexts(sent), "the shown answer is finished")
	assert.True(t, storedRow(t, b, groupChatID, "Noted,\n\nyour PIN is 9876.").Aside, "and stored aside")
	assert.Empty(t, memoryTexts(b, groupChatID))
}

// A photo turn of Mallory's that has shown nothing when she clears her
// messages is stopped, and its uploads are deleted.
func TestClear_OwnClearStopsTheirPhotoTurnAndDeletesItsUploads(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	sent := recordSends(tg)
	files := &recordingFiles{}
	asked, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/files") {
			files.handle(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		events := pinStream()
		_, _ = io.WriteString(w, sseFrames(events[:1])) // message_start: nothing shown
		w.(http.Flusher).Flush()
		asked <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, sseFrames(events[1:]))
	}))
	t.Cleanup(srv.Close)
	b.anthropicClient = anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	serveTelegramFiles(t, tg, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			ID: 920, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: mallory,
			Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}})
	}()
	<-asked

	b.handleUpdate(context.Background(), nil, commandUpdate(groupChatID, 921, mallory, "/clear"))
	close(release)
	<-done
	waitIdle(t, b, groupChatID)

	assert.Equal(t, []string{ownClearedNotice}, sentTexts(sent), "nothing answers the cleared photo")
	assert.Equal(t, []string{"file_rec_1"}, files.deletes(), "and its upload is deleted")
}

// hookWriter runs fn once, the first time a written line contains match.
type hookWriter struct {
	match string
	once  sync.Once
	fn    func()
}

func (w *hookWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.once.Do(w.fn)
	}
	return len(p), nil
}

// Blind r4 #1 inverted (K2): an admin's /clear of a member's messages in a
// group lands after that member's voice turn checked for a clear for the last
// time, as it falls back to a text reply, and before the reply is filed: the
// reply is neither filed, nor sent, nor in the next request.
func TestClear_UserClearJustBeforeFilingFilesNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	pin := evStream(evTextBlock(0, "Noted, your PIN is 9876."), evMessageEnd("end_turn"))
	stub := newAnthropicStub(t, pin, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	member := createUserWithRole(t, b, 206, "member", "admin") // admin: may use voice
	(&voiceStubs{text: "my PIN is 9876, remember it"}).install(t, b, tg)
	storeTextFrom(t, b, groupChatID, alice, "hi all")
	answerInTurn(t, b, groupChatID, "Hello!")
	var running *chatTurn
	hook := &hookWriter{match: "Error generating speech", fn: func() { // the turn's last check is behind it
		b.turnsMu.Lock()
		running = b.turns[chatKey(groupChatID)]
		b.turnsMu.Unlock()
		b.handleUpdate(context.Background(), nil, privateCommand(owner, 701, fmt.Sprintf("/clear %d %d", member.ID, groupChatID)))
	}}
	prev := ErrorLogger
	ErrorLogger = log.New(hook, "", 0)
	t.Cleanup(func() { ErrorLogger = prev })

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 700, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: member,
		Voice: &models.Voice{FileID: "v"}}})
	waitIdle(t, b, groupChatID)

	require.NotNil(t, running)
	assert.Equal(t, cancelClear, b.turnReason(running))
	assert.NotContains(t, sentTexts(sent), "Noted, your PIN is 9876.", "not sent")
	assert.NotContains(t, memoryTexts(b, groupChatID), "Noted, your PIN is 9876.", "not filed")
	b.handleUpdate(context.Background(), nil, textUpdate(groupChatID, 702, alice, "what's new?"))
	waitIdle(t, b, groupChatID)
	require.Len(t, stub.requestBetas(), 2)
	assert.False(t, requestHas(t, stub, 1, "9876"), "nor in the next request")
}

// K2: once a turn's delivery began, a restart no longer lands: in draft mode
// an edit that arrives while the draft closes, before the final send, is a
// correction of the record only.
func TestRunTurn_NoRestartOnceDeliveryBegan(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	b.config.StreamDrafts = true
	closing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tg.SendMessageDraftFunc = func(ctx context.Context, _ *bot.SendMessageDraftParams) (bool, error) {
		<-ctx.Done() // the turn closes its draft
		once.Do(func() { close(closing) })
		<-release
		return false, ctx.Err()
	}
	first := storeUserText(t, b, ownerChat, "hi")
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(ownerChat)) }()
	<-closing // delivery began: the draft is closing before the final send

	assert.False(t, b.restartForEdit(textReq(ownerChat), first.Seq), "the edit does not restart the turn")
	close(release)
	<-done
	waitIdle(t, b, ownerChat)
	assert.Equal(t, []string{"Open the Farm tab."}, sentTexts(sent))
	assert.Len(t, stub.requestBetas(), 1)
}

// Scoped r5 P9 inverted (K7, controller decision): a G9 aside reply leaves
// Alice's answered question with no reply after it; Bob's sticker, whose
// ticket came before that question, joins strictly by its ticket: before the
// question, so the sticker's request does not read it.
func TestSticker_JoinsByItsTicketAfterAnAsideReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStub(t, pinStream(), 1, 0, 1)
	stub.splitAt(1, 4) // the rerun shows "Noted," then waits: committed
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	recordSends(tg)
	ctx := context.Background()
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	bob := &models.User{ID: 204, Username: "bob"}

	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 60, mallory, "my PIN is 9876"))
	stickerDone := make(chan struct{})
	go func() { defer close(stickerDone); b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 61, bob)) }()
	stub.waitRequests(t, 1) // Mallory's batch runs ahead of the sticker, nothing shown
	waitForMediaWaiter(t, b, groupChatID)
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 62, alice, "and what is mine?"))
	flushPending(t, b, groupChatID) // restarts it: the rerun reads both
	stub.waitRequests(t, 2)
	require.Eventually(t, func() bool {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		tr := b.turns[chatKey(groupChatID)]
		return tr != nil && tr.committed
	}, 5*time.Second, time.Millisecond, "the rerun showed its first segment")
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 63, mallory, "/clear")) // its reply goes aside
	stub.open(1)
	<-stickerDone
	waitIdle(t, b, groupChatID)

	require.True(t, storedRow(t, b, groupChatID, "Noted,\n\nyour PIN is 9876.").Aside)
	n := len(stub.requestBetas())
	require.Equal(t, 3, n)
	_, last := stub.lastMessage(t, n-1)
	assert.Equal(t, "Sent a sticker: 😀", last)
	assert.False(t, requestHas(t, stub.anthropicStub, n-1, "and what is mine?"),
		"the sticker's request does not read Alice's question, which came after its ticket")
}

// Scoped r5 P3 inverted (K5): the owner's voice note takes over the owner's
// waiting text at its admission; a /clear lands before the note joins. The
// note does not join, and the batch of the cleared history does not come
// back to the quiet window: nothing of it is answered after the clear.
func TestClear_BeforeAFoldedMediaJoinsDropsTheBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	ctx := context.Background()
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	var calls atomic.Int32
	reached, hold := make(chan struct{}), make(chan struct{})
	tg.SendChatActionFunc = func(context.Context, *bot.SendChatActionParams) (bool, error) {
		if calls.Add(1) == 2 { // the voice turn's typing: after its admission, before its join
			close(reached)
			<-hold
		}
		return true, nil
	}

	b.handleUpdate(ctx, nil, textUpdate(ownerChat, 30, owner, "OLD-HISTORY question"))
	voiceDone := make(chan struct{})
	go func() {
		defer close(voiceDone)
		b.handleUpdate(ctx, nil, &models.Update{Message: voiceMessage(ownerChat, 31)})
	}()
	<-reached
	require.False(t, pendingBatch(b, ownerChat), "the note took the batch over")
	b.handleUpdate(ctx, nil, privateCommand(owner, 32, "/clear"))
	close(hold)
	<-voiceDone
	waitIdle(t, b, ownerChat)

	assert.False(t, pendingBatch(b, ownerChat), "the cleared history's batch is not back")
	assert.Empty(t, stub.requestBetas(), "nothing of it is answered")
}

// Scoped r5 P5 inverted (K6): a text, sticker or voice note whose join write
// fails is not dropped silently: its sender is told.
func TestJoin_AFailedWriteTellsTheSender(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const notice = "Sorry, I had trouble saving your message."
	for name, update := range map[string]*models.Update{
		"text":    textUpdate(groupChatID, 40, alice, "are you there?"),
		"sticker": rv4Sticker(groupChatID, 41, alice),
		"voice": {Message: &models.Message{ID: 42, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup},
			From: owner, Voice: &models.Voice{FileID: "v"}}},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, 123)
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			sent := recordSends(tg)
			captureErrorsLocked(t)
			(&voiceStubs{text: transcript}).install(t, b, tg)
			require.NoError(t, b.db.Callback().Update().Before("gorm:begin_transaction").Register("test:busy", func(db *gorm.DB) {
				if m, ok := db.Statement.Dest.(map[string]any); ok && m["aside"] == false {
					_ = db.AddError(errors.New("database is locked"))
				}
			}))

			b.handleUpdate(context.Background(), nil, update)
			waitIdle(t, b, groupChatID)

			assert.Equal(t, []string{notice}, sentTexts(sent))
			assert.Empty(t, stub.requestBetas())
		})
	}
}

// Scoped r6 Q1 inverted (L4, white-box): runStickerTurn's steps, with
// Mallory's own /clear landing as soon as the turn's snapshot step lets go of
// memory (afterSnapshot), the earliest it can once the turn read it. The turn
// knows its media message from its admission, so the clear stops it: no reply
// to the cleared sticker is sent or filed.
func TestClear_OwnClearInsideTheMediaSnapshotStopsTheTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStubOf(t, [][]string{evStream(evTextBlock(0, "Hello!"), evMessageEnd("end_turn")),
		evStream(evTextBlock(0, niceSticker), evMessageEnd("end_turn"))}, 1)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sends := &hookedSends{}
	sends.install(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	b.handleUpdate(ctx, nil, textUpdate(groupChatID, 899, alice, "warm-up")) // users exist; the chat is answered
	waitIdle(t, b, groupChatID)
	upd := rv4Sticker(groupChatID, 900, mallory)
	to := targetFor(upd.Message, "")
	userMsg, err := b.screenIncomingMessage(upd.Message, targetFor(upd.Message, upd.Message.BusinessConnectionID))
	require.NoError(t, err)

	media, ok := b.beginMediaTurn(ctx, to, mallory.ID, userMsg.ID)
	require.True(t, ok)
	turn := media.turn
	joined, outcome := b.joinMedia(userMsg, &media)
	require.Equal(t, joinOK, outcome)
	clearAfterSnapshot(t, turn, func() { b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 901, mallory, "/clear")) })
	contextMessages, found := b.snapshotForMedia(turn, b.getOrCreateChatMemory(chatKey(groupChatID)), joined.Seq)
	require.True(t, found)
	stopped := b.clearedTurn(turn)
	b.handleStickerMessage(media.ctx, turn, to, userMsg, upd.Message, contextMessages, true)
	b.endTurn(turn)
	waitIdle(t, b, groupChatID)

	assert.True(t, stopped, "her clear stops her sticker's turn")
	assert.NotContains(t, sends.texts(), niceSticker, "no reply to the cleared sticker is sent")
	assert.NotContains(t, memoryTexts(b, groupChatID), niceSticker, "nor filed")
}

// Scoped r6 Q2 inverted (L5, white-box): as above, with Alice's unanswered
// message, with no turn of its own, before the sticker. The stopped turn's
// rerun collects it (what lies before the media message), and runs: Alice is
// answered.
func TestClear_OwnClearBeforeTheMediaSnapshotKeepsOthersText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newHoldingStubOf(t, [][]string{evStream(evTextBlock(0, "Hello, Alice!"), evMessageEnd("end_turn"))}, 1)
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub.anthropicStub)
	oneConnection(t, b)
	sends := &hookedSends{}
	sends.install(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 898, alice, "/whoami")) // users exist
	b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 897, mallory, "/whoami"))
	storeTextFrom(t, b, groupChatID, alice, "anyone here?") // unanswered, with no turn of its own
	upd := rv4Sticker(groupChatID, 900, mallory)
	to := targetFor(upd.Message, "")
	userMsg, err := b.screenIncomingMessage(upd.Message, targetFor(upd.Message, upd.Message.BusinessConnectionID))
	require.NoError(t, err)

	media, ok := b.beginMediaTurn(ctx, to, mallory.ID, userMsg.ID)
	require.True(t, ok)
	turn := media.turn
	joined, outcome := b.joinMedia(userMsg, &media)
	require.Equal(t, joinOK, outcome)
	clearAfterSnapshot(t, turn, func() { b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 901, mallory, "/clear")) })
	contextMessages, found := b.snapshotForMedia(turn, b.getOrCreateChatMemory(chatKey(groupChatID)), joined.Seq)
	require.True(t, found)
	stopped := b.clearedTurn(turn)
	b.handleStickerMessage(media.ctx, turn, to, userMsg, upd.Message, contextMessages, true)
	b.endTurn(turn)
	waitIdle(t, b, groupChatID)

	assert.True(t, stopped)
	assert.NotNil(t, storedRow(t, b, groupChatID, "anyone here?").AnsweredOn,
		"Alice's message, which only the stopped media turn covered, is answered")
	assert.Contains(t, sends.texts(), "Hello, Alice!")
}

// Scoped r6 Q3 inverted (L3): Mallory's own /clear lands while her sticker's
// reply, already filed, is being sent. The turn counts as shown from its
// reply's filing, so the clear leaves it to finish: the send it runs on is
// not cut off.
func TestClear_OwnClearDuringTheStickerReplySendDoesNotCutItOff(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(evTextBlock(0, niceSticker), evMessageEnd("end_turn")))
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	ctx := context.Background()
	sends := &hookedSends{match: niceSticker}
	sends.hook = func() { b.handleUpdate(ctx, nil, commandUpdate(groupChatID, 901, mallory, "/clear")) }
	sends.install(tg)

	b.handleUpdate(ctx, nil, rv4Sticker(groupChatID, 900, mallory))
	waitIdle(t, b, groupChatID)

	require.True(t, sends.hooked.Load(), "the clear landed during the reply's send")
	assert.NoError(t, sends.ctxErr, "the clear does not cancel the send of a reply already filed")
	assert.Equal(t, []string{ownClearedNotice, niceSticker}, sends.texts())
}

// Scoped r6 Q4 inverted (L3): the same for a voice note's audio reply: the
// owner's /clear of the member's messages lands while the audio, its text
// already filed, is sent.
func TestClear_ClearDuringTheVoiceAudioSendDoesNotCutItOff(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(evTextBlock(0, "Table booked."), evMessageEnd("end_turn")))
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	b.config.MessagePerHour, b.config.MessagePerDay = 1000, 10000
	member := createUserWithRole(t, b, 206, "member", "admin") // may use voice
	(&voiceStubs{text: transcript}).install(t, b, tg)
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ID3-audio")
	}))
	t.Cleanup(tts.Close)
	b.ttsURL = tts.URL + "/"
	var audioCtxErr error
	var audioSent atomic.Bool
	tg.SendAudioFunc = func(ctx context.Context, _ *bot.SendAudioParams) (*models.Message, error) {
		b.handleUpdate(context.Background(), nil, privateCommand(owner, 701, fmt.Sprintf("/clear %d %d", member.ID, groupChatID)))
		audioCtxErr = ctx.Err()
		audioSent.Store(true)
		return &models.Message{}, nil
	}

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 700, Chat: models.Chat{ID: groupChatID, Type: models.ChatTypeSupergroup}, From: member,
		Voice: &models.Voice{FileID: "v"}}})
	waitIdle(t, b, groupChatID)

	require.True(t, audioSent.Load(), "the clear landed during the audio's send")
	assert.NoError(t, audioCtxErr, "the clear does not cancel the send of a reply already filed")
}

// Blind r5 #1 and scoped r6 Q7 inverted (L1): photo 1 of an album is
// uploading, its file already created on the Files API's side, while photo 2
// is still downloading, when photo 2 ends: a /clear deleted the album, or its
// download fails. Photo 1's upload is not cut off (its file id would never
// come back, and the file would stay on the Files API with no record of it):
// the album waits for it, then deletes it.
func TestPhoto_AnAlbumItemThatEndsDoesNotCutOffASiblingsUpload(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, ending := range []string{"cleared", "its download fails"} {
		t.Run(ending, func(t *testing.T) {
			files := &recordingFiles{}
			uploadStarted, releaseUpload := files.holdUploads(t)
			b, tg := setupBotForTest(t, ownerChat)
			oneConnection(t, b)
			sent := recordSends(tg)
			captureErrorsLocked(t)
			b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
			serveTelegramFiles(t, tg, nil)
			inner := tg.GetFileFunc
			p2Entered, p2Gate := make(chan struct{}, 1), make(chan struct{})
			var gateOnce sync.Once
			openP2 := func() { gateOnce.Do(func() { close(p2Gate) }) }
			t.Cleanup(openP2)
			tg.GetFileFunc = func(ctx context.Context, p *bot.GetFileParams) (*models.File, error) {
				if p.FileID == "p2" {
					p2Entered <- struct{}{}
					select {
					case <-p2Gate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					if ending == "its download fails" {
						return nil, errors.New("telegram: the file is gone")
					}
				}
				return inner(ctx, p)
			}
			item := func(id int, fileID string) *models.Update {
				return &models.Update{Message: &models.Message{
					ID: id, Chat: models.Chat{ID: ownerChat, Type: models.ChatTypePrivate}, From: owner,
					MediaGroupID: "g1", Caption: "wrong album, private scans",
					Photo: []models.PhotoSize{{FileID: fileID, Width: 1, Height: 1}}}}
			}
			b.handleUpdate(context.Background(), nil, item(9600, "p1"))
			b.handleUpdate(context.Background(), nil, item(9601, "p2"))
			done := make(chan struct{})
			go func() {
				defer close(done)
				b.flushAlbum(context.Background(), albumKey{conv: chatKey(ownerChat), mediaGroupID: "g1"})
			}() // before its timer
			<-uploadStarted // photo 1's upload reached the Files API
			<-p2Entered     // photo 2 is still downloading
			want := []string{"Sorry, I couldn't process one of your images."}
			if ending == "cleared" {
				b.handleUpdate(context.Background(), nil, privateCommand(owner, 9602, "/clear"))
				want = []string{clearedNotice}
			}
			openP2() // photo 2 ends
			select {
			case <-done:
				t.Error("the album returned with photo 1's upload still in flight: it was cut off")
			case <-time.After(300 * time.Millisecond):
			}
			releaseUpload()
			<-done
			waitIdle(t, b, ownerChat)

			assert.Equal(t, 1, files.uploads())
			assert.Equal(t, []string{"file_rec_1"}, files.deletes(), "photo 1's upload came back, and is deleted")
			assert.Equal(t, want, sentTexts(sent))
		})
	}
}

// Blind r4 #3 inverted (K4): a photo whose row a clear deleted while the
// photo was being downloaded is not uploaded afterwards: no upload, so none
// can be orphaned when a compensating delete fails. The check covers the
// clear's cutoff, in force before its rows go, and the row itself.
func TestClear_PhotoDownloadingAcrossTheClearIsNotUploaded(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, window := range []string{"the clear", "its cutoff, before its rows go", "its row, gone without a clear"} {
		t.Run(window, func(t *testing.T) {
			files := &recordingFiles{}
			b, tg := setupBotForTest(t, ownerChat)
			oneConnection(t, b)
			sent := recordSends(tg)
			captureErrorsLocked(t)
			b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
			started, release := holdDownloads(t, tg)
			done := make(chan struct{})
			var fired atomic.Bool
			if window == "its cutoff, before its rows go" {
				// Before the clear's delete takes the one connection: the
				// photo, released now, finishes against the cutoff alone.
				require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:cutoff_only", func(db *gorm.DB) {
					if fired.CompareAndSwap(false, true) {
						release()
						select {
						case <-done:
						case <-time.After(10 * time.Second):
						}
					}
				}))
			}
			go func() {
				defer close(done)
				b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
					ID: 9500, Chat: models.Chat{ID: ownerChat, Type: models.ChatTypePrivate}, From: owner,
					Caption: "private document scan", Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}})
			}()
			<-started
			want := []string{clearedNotice}
			if window == "its row, gone without a clear" {
				require.NoError(t, b.db.Unscoped().Where("chat_id = ?", ownerChat).Delete(&Message{}).Error)
				want = nil
			} else {
				b.handleUpdate(context.Background(), nil, privateCommand(owner, 9501, "/clear"))
			}
			release()
			<-done
			waitIdle(t, b, ownerChat)

			assert.Equal(t, window == "its cutoff, before its rows go", fired.Load(), "the photo finished inside the clear's step")
			assert.Zero(t, files.uploads(), "the cleared photo is not uploaded")
			assert.Equal(t, want, sentTexts(sent), "and nothing is said about it")
		})
	}
}

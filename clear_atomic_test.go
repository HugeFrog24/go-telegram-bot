package main

import (
	"context"
	"fmt"
	"io"
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

// /clear and /clear_hard are atomic with respect to turns and replies: one
// step under the memory lock swaps in an empty memory with a fresh
// generation, cancels the running turn for good and records the highest
// message id as the cutoff; the delete then covers ids up to the cutoff.

// clearOwnChat runs the owner's bare /clear (or /clear_hard) in its private chat.
func clearOwnChat(b *Bot, chatID int64, hard bool) {
	b.clearChatHistory(context.Background(), replyTarget{chatID: chatID}, profile{id: 123}, 0, 0, hard)
}

// rowsWithText is every row, deleted or not, whose text contains s.
func rowsWithText(t *testing.T, b *Bot, chatID int64, s string) []Message {
	t.Helper()
	var rows []Message
	require.NoError(t, b.db.Unscoped().Where("chat_id = ? AND text LIKE ?", chatID, "%"+s+"%").Find(&rows).Error)
	return rows
}

// midDelete runs during /clear's first message delete, while it holds the
// database connection: what a turn finishing at that moment does.
func midDelete(t *testing.T, b *Bot, during func()) <-chan struct{} {
	t.Helper()
	var once sync.Once
	done := make(chan struct{})
	require.NoError(t, b.db.Callback().Delete().After("gorm:delete").Register("test:mid_clear_"+t.Name(), func(db *gorm.DB) {
		if _, isMsg := db.Statement.Model.(*Message); !isMsg {
			return
		}
		once.Do(func() {
			go func() { defer close(done); during() }()
			time.Sleep(150 * time.Millisecond) // the turn gets as far as it can while the delete holds the connection
		})
	}))
	return done
}

// Blind B6 and B6b inverted: a reply recorded while /clear's delete runs
// belongs to the cleared history. It is neither stored nor sent, and the next
// conversation never sees it, after a restart either.
func TestClear_ReplyRecordedDuringTheDeleteIsNotKept(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, hard := range map[string]bool{"/clear": false, "/clear_hard": true} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, 123)
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			sent := recordSends(tg)
			const chatID = int64(123)
			first := storeUserText(t, b, chatID, "my card PIN is 9876, remember it")
			turn, _, ok := b.beginTextTurn(textReq(chatID), first.Seq)
			require.True(t, ok)
			b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatKey(chatID)))
			var recordErr error
			done := midDelete(t, b, func() {
				_, recordErr = b.recordTurnReply(turn, chatKey(chatID), "Noted: your PIN is 9876.")
				b.finishTurn(turn)
			})

			clearOwnChat(b, chatID, hard)
			<-done

			assert.ErrorIs(t, recordErr, errStaleReply, "the reply answers the cleared history")
			assert.Empty(t, rowsWithText(t, b, chatID, "Noted"), "it is not stored, not even aside")
			assert.NotContains(t, sentTexts(sent), "Noted: your PIN is 9876.")
			dropChatMemory(b, chatID) // a restart
			storeUserText(t, b, chatID, "hello again")
			b.respondToChat(textReq(chatID))
			require.Len(t, stub.requestBetas(), 1)
			assert.False(t, requestHas(t, stub, 0, "9876"), "the next request holds nothing of the cleared history")
		})
	}
}

// Blind B7 inverted: a text turn that registers after the clear, for a flush
// whose want was read before it, ends silently when its snapshot shows the
// memory changed: no "BUG", no error reply, nothing filed.
func TestClear_TurnRegisteredAfterTheClearForAnOldFlushEndsSilently(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	errs := captureErrors(t)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "hi")
	want, gen := b.flushWant(chatKey(chatID)) // the flush read memory before the clear
	req := textReq(chatID)
	req.memoryGen = gen

	clearOwnChat(b, chatID, false)
	// The flush got past its own checks before the swap, and registers now.
	b.turnsMu.Lock()
	turn, turnCtx := b.registerTurnLocked(req.ctx, chatKey(chatID), want, false)
	b.turnsMu.Unlock()
	b.runTextTurn(turn, turnCtx, req)
	waitIdle(t, b, chatID)

	assert.Empty(t, stub.requestBetas())
	assert.NotContains(t, errs.String(), "BUG")
	assert.Equal(t, []string{clearedNotice}, sentTexts(sent))
	assert.Empty(t, memoryTexts(b, chatID))
}

// Scoped N3a and N3b inverted: a flush whose want was read before /clear and
// that reaches beginTextTurn after it is dropped silently, whether a turn
// still runs (it is not collected) or not (it registers nothing).
func TestClear_FlushFromBeforeTheClearIsDropped(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, running := range map[string]bool{"a turn still running": true, "no turn running": false} {
		t.Run(name, func(t *testing.T) {
			stub := newAnthropicStub(t, candleFarmStream())
			b, tg := setupBotForTest(t, 123)
			useAnthropicStub(b, stub)
			oneConnection(t, b)
			sent := recordSends(tg)
			errs := captureErrors(t)
			const chatID = int64(123)
			storeUserText(t, b, chatID, "first")
			var turn *chatTurn
			if running {
				turn, _ = b.beginTurn(context.Background(), chatID)
				b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatKey(chatID)))
			}
			storeUserText(t, b, chatID, "second, joined before the clear")
			want, gen := b.flushWant(chatKey(chatID))
			req := textReq(chatID)
			req.memoryGen = gen

			clearOwnChat(b, chatID, false)
			_, _, ok := b.beginTextTurn(req, want)
			assert.False(t, ok)
			if running {
				b.endTurn(turn)
			}
			waitIdle(t, b, chatID)

			assert.Empty(t, stub.requestBetas())
			assert.NotContains(t, errs.String(), "BUG")
			assert.Equal(t, []string{clearedNotice}, sentTexts(sent))
			assert.Empty(t, memoryTexts(b, chatID))
		})
	}
}

// The handover checks the generation too: a flush collected before /clear
// that slipped past the clear's own drop gets no turn.
func TestCloseTurn_NoHandoverForAFlushFromAnotherGeneration(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(823)
	_, gen := b.flushWant(chatKey(chatID))
	turn, _, ok := b.beginTextTurn(textReq(chatID), 1)
	require.True(t, ok)
	b.markCommitted(turn)
	stale := textReq(chatID)
	stale.memoryGen = gen
	b.turnsMu.Lock()
	turn.rerun, turn.rerunWant = &stale, 7 // collected in the old generation
	b.turnsMu.Unlock()
	b.chatMemoriesMu.Lock()
	b.memoryGen++
	b.chatMemories[chatKey(chatID)] = &ChatMemory{Size: b.memorySize * 2, generation: b.memoryGen} // /clear's swap
	b.chatMemoriesMu.Unlock()

	_, next, _ := b.closeTurn(turn, true)

	assert.Nil(t, next)
	assert.Nil(t, heldTurn(b, chatID))
}

// Scoped N2 inverted: cancelClear is sticky. A Stop tap or an edit after
// /clear leaves it, so nothing of the cleared turn is delivered; a new flush
// is still collected and runs as a fresh turn.
func TestClear_CancelIsSticky(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	const chatID = int64(123)
	turn, _, ok := b.beginTextTurn(textReq(chatID), 1)
	require.True(t, ok)
	defer b.finishTurn(turn)
	b.setTurnDraft(turn, turn.draftIDString())
	_, err := b.beginClear(chatKey(chatID))
	require.NoError(t, err)

	assert.False(t, b.stopTurn(chatKey(chatID), int(turn.seq)), "a Stop tap after the clear stops nothing")
	assert.False(t, b.restartForEdit(textReq(chatID), 1), "an edit restarts nothing")
	after := storeUserText(t, b, chatID, "a new message after the clear")
	req := textReq(chatID)
	_, _, ok = b.beginTextTurn(req, after.Seq)
	assert.False(t, ok, "a new flush is collected behind the cleared turn")
	assert.True(t, b.clearedTurn(turn), "the turn still reads as cleared")
	b.turnsMu.Lock()
	collected := turn.rerun != nil
	b.turnsMu.Unlock()
	assert.True(t, collected, "and runs as a fresh turn afterwards")
}

// Scoped N4 inverted: a voice note stored before /clear and waiting behind a
// running turn is not answered after it: its row is gone, so it never joins.
func TestClear_VoiceNoteWaitingThroughTheClearIsNotAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	sent := recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "earlier")
	running, _ := b.beginTurn(context.Background(), chatID)
	voiceDone := make(chan struct{})
	go func() {
		defer close(voiceDone)
		b.handleUpdate(context.Background(), nil, &models.Update{Message: voiceMessage(chatID, 1010)})
	}()
	waitForMediaWaiter(t, b, chatID) // stored, transcribed, and waiting for the chat

	clearOwnChat(b, chatID, false)
	b.endTurn(running)
	<-voiceDone
	waitIdle(t, b, chatID)

	assert.Empty(t, stub.requestBetas(), "the pre-clear voice note is not answered")
	assert.Equal(t, []string{clearedNotice}, sentTexts(sent))
	assert.Empty(t, memoryTexts(b, chatID))
	dropChatMemory(b, chatID)
	assert.Empty(t, memoryTexts(b, chatID), "nor does a reload bring anything back")
}

// slowFiles is a Files API that holds every delete until release closes.
type slowFiles struct {
	deleting chan struct{}
	release  chan struct{}
}

func (f *slowFiles) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/v1/files/") {
			select {
			case f.deleting <- struct{}{}:
			default:
			}
			<-f.release
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"file_x","type":"file_deleted"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// /clear_hard deletes, rows and Files API files, exactly the set up to its
// cutoff: a reply the running turn tries to record while the deletions run is
// neither stored nor sent, and a message that arrives meanwhile is kept and
// starts the new conversation.
func TestClearHard_WhileFilesAreDeletedKeepsTheCutoffExact(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	sent := recordSends(tg)
	files := &slowFiles{deleting: make(chan struct{}, 1), release: make(chan struct{})}
	b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
	const chatID = int64(123)
	photo := storePhoto(t, b, chatID, "a photo")
	storeUserText(t, b, chatID, "and a question")
	turn, _, ok := b.beginTextTurn(textReq(chatID), b.newestAnswerableSeqInChat(chatKey(chatID)))
	require.True(t, ok)
	b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatKey(chatID)))

	cleared := make(chan struct{})
	go func() { defer close(cleared); clearOwnChat(b, chatID, true) }()
	<-files.deleting // the soft delete is done; the Files API deletion hangs
	_, recordErr := b.recordTurnReply(turn, chatKey(chatID), "an answer to the cleared photo")
	b.finishTurn(turn)
	newcomer := storeUserText(t, b, chatID, "a new start")
	close(files.release)
	<-cleared

	assert.ErrorIs(t, recordErr, errStaleReply)
	assert.Empty(t, rowsWithText(t, b, chatID, "an answer to the cleared photo"), "the reply is not stored")
	assert.Empty(t, rowsWithText(t, b, chatID, "a photo"), "the photo row is hard-deleted")
	assert.Empty(t, rowsWithText(t, b, chatID, "and a question"), "and so is the question")
	require.Len(t, rowsWithText(t, b, chatID, "a new start"), 1, "the newcomer survives")
	assert.Equal(t, []string{"a new start"}, memoryTexts(b, chatID))
	assert.Equal(t, []string{clearedNotice}, sentTexts(sent))
	assert.Greater(t, newcomer.ID, photo.ID)
}

// Stress: text, stickers, edits and /clear across chats at once, under
// -race. Every handler returns, and nothing hits the BUG path.
func TestClear_ConcurrentStress(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	oneConnection(t, b)
	recordSends(tg)
	errs := captureErrors(t)
	b.config.MessagePerHour, b.config.MessagePerDay = 100000, 100000
	b.config.DebounceMs = 5
	chats := []int64{123, 124, 125}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				chat := chats[(w+i)%len(chats)]
				u := &models.User{ID: chat, Username: fmt.Sprintf("u%d", w)} // the chat's own user: a private chat
				id := w*1000 + i
				var upd *models.Update
				switch i % 5 {
				case 0, 1:
					upd = textUpdate(chat, id, u, fmt.Sprintf("hi %d", id))
				case 2:
					upd = &models.Update{Message: &models.Message{ID: id, Chat: models.Chat{ID: chat}, From: u,
						Sticker: &models.Sticker{FileID: "s", Emoji: "😀"}}}
				case 3:
					upd = &models.Update{EditedMessage: &models.Message{ID: id - 3, Chat: models.Chat{ID: chat}, From: u, Text: "edited"}}
				case 4:
					upd = commandUpdate(chat, id, u, "/clear")
				}
				b.handleUpdate(context.Background(), nil, upd)
			}
		}(w)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("handlers did not finish: deadlock?")
	}
	for _, c := range chats {
		waitIdle(t, b, c)
	}
	assert.NotContains(t, errs.String(), "BUG")
}

// beginClear swaps in an empty memory, it does not just drop the old one: a
// read before the rows are deleted finds the empty memory, so no reload can
// bring the cleared rows back, and the cutoff is the highest id so far.
func TestBeginClear_SwapsInAnEmptyMemoryBeforeAnyRowGoes(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := historyTestBot(t, nil)
	const chatID = int64(824)
	storeUserText(t, b, chatID, "before the clear")
	last := storeUserText(t, b, chatID, "also before the clear")
	before := b.currentGeneration(chatKey(chatID))

	cutoff, err := b.beginClear(chatKey(chatID)) // the rows are not deleted yet

	require.NoError(t, err)
	assert.Equal(t, last.ID, cutoff)
	assert.Empty(t, memoryTexts(b, chatID), "the old rows do not come back")
	assert.NotEqual(t, before, b.currentGeneration(chatKey(chatID)), "a fresh generation")
}

// Scoped R5 inverted: in draft mode, the stream has ended and the turn is
// closing its draft, whose last call is still out (a Telegram round trip that
// does not return at once on cancel). /clear lands then: the reply to the
// cleared history is not sent.
func TestClear_DuringTheLastDraftCallSendsNothing(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
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
		<-release // the call in flight returns only now
		return false, ctx.Err()
	}
	storeUserText(t, b, ownerChat, "my PIN is 9876")
	done := make(chan struct{})
	go func() { defer close(done); b.respondToChat(textReq(ownerChat)) }()
	<-closing // the stream is over; finish waits for the draft call

	b.handleUpdate(context.Background(), nil, privateCommand(owner, 50, "/clear"))
	close(release)
	<-done
	waitIdle(t, b, ownerChat)

	assert.Equal(t, []string{clearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.Empty(t, memoryTexts(b, ownerChat))
}

// Scoped R2 inverted: a message sent right after /clear_hard, while its Files
// API deletions run, joins the new memory and waits in its quiet window; the
// clear does not drop that batch: it dropped the chat's waiting batch in its
// one step, before the deletions.
func TestClearHard_MessageDuringTheDeletionsKeepsItsBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	files := &slowFiles{deleting: make(chan struct{}, 1), release: make(chan struct{})}
	b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	storePhoto(t, b, ownerChat, "a photo")

	cleared := make(chan struct{})
	go func() {
		defer close(cleared)
		b.handleUpdate(context.Background(), nil, privateCommand(owner, 500, "/clear_hard"))
	}()
	<-files.deleting // the clear is running its Files API deletions
	b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 501, owner, "a new question after the clear"))
	require.True(t, pendingBatch(b, ownerChat), "the new question waits in its quiet window")
	close(files.release)
	<-cleared

	assert.Equal(t, []string{"a new question after the clear"}, memoryTexts(b, ownerChat))
	assert.True(t, pendingBatch(b, ownerChat), "its batch is still waiting, to be answered")
}

// Scoped R2b inverted: the same for a soft /clear, the message landing while
// its delete runs.
func TestClear_MessageDuringTheDeleteKeepsItsBatch(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, 123)
	oneConnection(t, b)
	recordSends(tg)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	storeUserText(t, b, ownerChat, "old")
	var once sync.Once
	require.NoError(t, b.db.Callback().Delete().Before("gorm:begin_transaction").Register("test:during_clear", func(db *gorm.DB) {
		if _, isMsg := db.Statement.Model.(*Message); !isMsg {
			return
		}
		once.Do(func() {
			arrived := make(chan struct{})
			go func() {
				defer close(arrived)
				b.handleUpdate(context.Background(), nil, textUpdate(ownerChat, 601, owner, "hello again"))
			}()
			<-arrived
		})
	}))

	b.handleUpdate(context.Background(), nil, privateCommand(owner, 600, "/clear"))

	assert.Equal(t, []string{"hello again"}, memoryTexts(b, ownerChat))
	assert.True(t, pendingBatch(b, ownerChat), "its batch is still waiting, to be answered")
}

// recordingFiles is a Files API that records each delete; every upload gets a
// fresh id. The model is not served.
type recordingFiles struct {
	mu      sync.Mutex
	next    int
	deleted []string
	// gate, once set (holdUploads), holds each upload until it is closed.
	gate, started chan struct{}
}

// holdUploads holds each upload, once it reached the server, until release.
func (f *recordingFiles) holdUploads(t *testing.T) (started chan struct{}, release func()) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate, f.started = make(chan struct{}), make(chan struct{}, 8)
	var once sync.Once
	gate := f.gate
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return f.started, release
}

func (f *recordingFiles) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

// handle serves one Files API call; anything else is not served.
func (f *recordingFiles) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	gate, started := f.gate, f.started
	f.mu.Unlock()
	if gate != nil && r.Method == http.MethodPost {
		started <- struct{}{}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/files"):
		f.next++
		_, _ = fmt.Fprintf(w, `{"id":"file_rec_%d","type":"file","filename":"photo.jpg","mime_type":"image/jpeg",`+
			`"size_bytes":4,"created_at":"2026-11-16T00:00:00Z","downloadable":false}`, f.next)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/v1/files/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.deleted = append(f.deleted, id)
		_, _ = fmt.Fprintf(w, `{"id":%q,"type":"file_deleted"}`, id)
	default:
		http.Error(w, "not served", http.StatusTeapot)
	}
}

// uploads is how many files were uploaded.
func (f *recordingFiles) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

func (f *recordingFiles) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// Scoped R3 and blind r3 #4 inverted: a photo sent before /clear or
// /clear_hard, still uploading when the clear runs, is not answered after
// it: its row was stored, aside, on arrival, so the clear deleted it; the
// photo does not join, and its upload is deleted.
func TestClear_PhotoUploadingAcrossTheClearDoesNotSurviveIt(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, command := range []string{"/clear", "/clear_hard"} {
		t.Run(command, func(t *testing.T) {
			b, tg := setupBotForTest(t, ownerChat)
			oneConnection(t, b)
			sent := recordSends(tg)
			files := &recordingFiles{}
			b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
			serveTelegramFiles(t, tg, nil)
			started, release := files.holdUploads(t)
			const caption = "OOPS-WRONG-PHOTO"

			done := make(chan struct{})
			go func() {
				defer close(done)
				b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
					ID: 9400, Chat: models.Chat{ID: ownerChat, Type: models.ChatTypePrivate}, From: owner,
					Caption: caption, Photo: []models.PhotoSize{{FileID: "p", Width: 1, Height: 1}}}})
			}()
			<-started // the photo is being uploaded
			b.handleUpdate(context.Background(), nil, privateCommand(owner, 9401, command))
			release()
			<-done
			waitIdle(t, b, ownerChat)

			assert.Equal(t, []string{clearedNotice}, sentTexts(sent), "nothing after the confirmation")
			assert.Empty(t, memoryTexts(b, ownerChat))
			assert.Empty(t, liveTexts(t, b, ownerChat, owner.ID), "its row went with the clear")
			assert.Equal(t, []string{"file_rec_1"}, files.deletes(), "and its upload is deleted")
		})
	}
}

// /clear landing after a photo's uploads were recorded on its row, before the
// photo joins: the join is refused, nothing is answered, and the uploads are
// deleted (G1's compensation).
func TestClear_PhotoClearedBeforeItsJoinHasItsUploadsDeleted(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := setupBotForTest(t, ownerChat)
	oneConnection(t, b)
	sent := recordSends(tg)
	files := &recordingFiles{}
	b.anthropicClient = anthropic.NewClient(option.WithBaseURL(files.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
	serveTelegramFiles(t, tg, nil)
	var fired atomic.Bool
	// After the uploads' write has committed and released the one connection.
	require.NoError(t, b.db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:clear_before_join", func(db *gorm.DB) {
		if m, ok := db.Statement.Dest.(*Message); ok && len(m.ImageFileIDs) > 0 && fired.CompareAndSwap(false, true) {
			clearOwnChat(b, ownerChat, false)
		}
	}))

	b.handleUpdate(context.Background(), nil, photoUpdate(9410))
	waitIdle(t, b, ownerChat)

	require.True(t, fired.Load())
	assert.Equal(t, []string{clearedNotice}, sentTexts(sent), "nothing after the confirmation")
	assert.Empty(t, memoryTexts(b, ownerChat))
	assert.Equal(t, []string{"file_rec_1"}, files.deletes(), "its upload is deleted")
}

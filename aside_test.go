package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const transcript = "please book a table for four at eight"

// textUpdate is a plain text message from user in chat.
func textUpdate(chatID int64, msgID int, user *models.User, text string) *models.Update {
	chat := models.Chat{ID: chatID}
	if chatID < 0 {
		chat.Type = models.ChatTypeSupergroup
	}
	return &models.Update{Message: &models.Message{ID: msgID, Chat: chat, From: user, Text: text}}
}

// commandUpdate is a message that starts with a bot command.
func commandUpdate(chatID int64, msgID int, user *models.User, text string) *models.Update {
	u := textUpdate(chatID, msgID, user, text)
	command, _, _ := strings.Cut(text, " ")
	u.Message.Entities = []models.MessageEntity{{Type: "bot_command", Offset: 0, Length: len(command)}}
	return u
}

// flushPending fires the chat's pending quiet window now.
func flushPending(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	b.intakeBuffersMu.Lock()
	pending, ok := b.intakeBuffers[chatID]
	var seq uint64
	if ok {
		seq = pending.seq
	}
	b.intakeBuffersMu.Unlock()
	require.True(t, ok, "chat %d has no pending batch", chatID)
	b.flushIntake(context.Background(), chatID, seq)
}

// storedRow is the chat's stored message with text, aside or not.
func storedRow(t *testing.T, b *Bot, chatID int64, text string) Message {
	t.Helper()
	var m Message
	require.NoError(t, b.db.Where("chat_id = ? AND text = ?", chatID, text).First(&m).Error, "no row %q", text)
	return m
}

// requestHas reports whether request i of stub contains s anywhere.
func requestHas(t *testing.T, stub *anthropicStub, i int, s string) bool {
	t.Helper()
	return strings.Contains(string(stub.body(t, i)), s)
}

// waitIdle waits until no turn runs on the chat, including one an ending turn
// started on its own goroutine for a collected message.
func waitIdle(t *testing.T, b *Bot, chatID int64) {
	t.Helper()
	require.Eventually(t, func() bool { return chatIdle(b, chatID) }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // a rerun is started on its own goroutine
	require.Eventually(t, func() bool { return chatIdle(b, chatID) }, 5*time.Second, 5*time.Millisecond)
}

// voiceStubs stands in for Telegram's file download and ElevenLabs. Speech
// to text answers with text, or fails while fail is set; text to speech
// always fails, so a voice turn falls back to a text reply. release, when
// set, holds every download until it is closed.
type voiceStubs struct {
	text    string
	release chan struct{}
}

func (v *voiceStubs) install(t *testing.T, b *Bot, tg *MockTelegramClient) {
	t.Helper()
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "OggS")
	}))
	t.Cleanup(files.Close)
	tg.GetFileFunc = func(ctx context.Context, _ *bot.GetFileParams) (*models.File, error) {
		if v.release != nil {
			select {
			case <-v.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &models.File{FilePath: "voice.ogg"}, nil
	}
	tg.FileDownloadLinkFunc = func(*models.File) string { return files.URL }
	stt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if v.text == "" {
			http.Error(w, "no speech", http.StatusUnprocessableEntity)
			return
		}
		_, _ = io.WriteString(w, `{"text":`+jsonString(v.text)+`}`)
	}))
	t.Cleanup(stt.Close)
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "tts off in tests", http.StatusServiceUnavailable)
	}))
	t.Cleanup(tts.Close)
	b.sttURL, b.ttsURL = stt.URL, tts.URL+"/"
	b.config.ElevenLabsAPIKey = "test"
}

// voiceMessage is a voice note from the bot's owner.
func voiceMessage(chatID int64, msgID int) *models.Message {
	return &models.Message{
		ID: msgID, Chat: models.Chat{ID: chatID}, From: &models.User{ID: 123, Username: "owner"},
		Voice: &models.Voice{FileID: "voice-file"},
	}
}

// R1 inverted: in a group, another member's command lands while a message
// waits in its quiet window. The command and its reply stay out of the
// conversation, so the waiting message is still answered, and is what the
// request ends with.
func TestHandleUpdate_CommandInGroupKeepsPendingFlush(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	const chatID = int64(-1001)
	asker := &models.User{ID: 4141, Username: "asker"}
	other := &models.User{ID: 4242, Username: "other"}

	b.handleUpdate(context.Background(), nil, textUpdate(chatID, 900, asker, "hi, where is the candle farm?"))
	b.handleUpdate(context.Background(), nil, commandUpdate(chatID, 901, other, "/whoami"))
	require.Len(t, tg.sent(), 1, "the command is answered")
	whoami := tg.sent()[0].Text
	flushPending(t, b, chatID)

	require.Len(t, stub.requestBetas(), 1, "the waiting message is answered")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "hi, where is the candle farm?", text)
	assert.False(t, requestHas(t, stub, 0, "/whoami"), "the command is not in the model's context")
	assert.False(t, requestHas(t, stub, 0, "Your Information"), "nor is its reply")
	assert.NotNil(t, storedRow(t, b, chatID, "hi, where is the candle farm?").AnsweredOn)
	assert.True(t, storedRow(t, b, chatID, "/whoami").Aside, "the command is stored aside")
	assert.True(t, storedRow(t, b, chatID, whoami).Aside, "and so is its reply")
	assert.Equal(t, []string{"hi, where is the candle farm?", "Open the Farm tab."}, memoryTexts(b, chatID))
}

// An unknown command is ordinary text for the model, as before.
func TestHandleUpdate_UnknownCommandIsText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	const chatID = int64(790)

	b.handleUpdate(context.Background(), nil, commandUpdate(chatID, 902, &models.User{ID: chatID}, "/farm where?"))

	require.Len(t, stub.requestBetas(), 1)
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "/farm where?", text)
	assert.False(t, storedRow(t, b, chatID, "/farm where?").Aside)
}

// A rate-limited message and its notice stay out of the conversation.
func TestHandleUpdate_RateLimitedMessageStaysOutOfContext(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.config.DebounceMs = int(bufferOnly / time.Millisecond)
	b.config.MessagePerHour = 1
	const chatID = int64(791)
	user := &models.User{ID: chatID, Username: "eager"}

	b.handleUpdate(context.Background(), nil, textUpdate(chatID, 903, user, "where is the candle farm?"))
	b.handleUpdate(context.Background(), nil, textUpdate(chatID, 904, user, "hello?? anyone??"))
	require.Len(t, tg.sent(), 1, "the second message gets the rate-limit notice")
	notice := tg.sent()[0].Text
	flushPending(t, b, chatID)

	require.Len(t, stub.requestBetas(), 1)
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "where is the candle farm?", text)
	assert.False(t, requestHas(t, stub, 0, "anyone??"), "the rate-limited message is not in context")
	assert.False(t, requestHas(t, stub, 0, notice), "nor is the notice")
	assert.True(t, storedRow(t, b, chatID, "hello?? anyone??").Aside)
	assert.True(t, storedRow(t, b, chatID, notice).Aside)
}

// Media the bot does not read yet is stored aside and never enters memory.
func TestHandleUpdate_UnreadMediaIsAside(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, tg := historyTestBot(t, nil)
	const chatID = int64(792)

	b.handleUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
		ID: 905, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID},
		Video: &models.Video{FileID: "video-file"},
	}})

	assert.Empty(t, tg.sent())
	assert.Empty(t, memoryTexts(b, chatID))
	var row Message
	require.NoError(t, b.db.Where("chat_id = ? AND telegram_message_id = ?", chatID, 905).First(&row).Error)
	assert.True(t, row.Aside)
}

// R2 inverted: a voice note is invisible to other turns until it is
// transcribed. A text turn that runs first answers the text alone, and the
// voice turn then answers the transcript.
func TestHandleVoiceMessage_InvisibleUntilTranscribed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	(&voiceStubs{text: transcript}).install(t, b, tg)
	const chatID = int64(123)
	storeUserText(t, b, chatID, "one more thing")
	voice := voiceMessage(chatID, 906)
	userMsg, err := b.screenIncomingMessage(voice) // the voice handler's intake
	require.NoError(t, err)

	b.respondToChat(textReq(chatID)) // e.g. a collected rerun wins the race to the turn slot

	require.Len(t, stub.requestBetas(), 1)
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "one more thing", text)
	assert.False(t, requestHas(t, stub, 0, "[Voice message]"), "the untranscribed voice note is not read")

	b.handleVoiceMessage(context.Background(), voice, userMsg, replyTarget{chatID: chatID},
		123, "owner", "", "", false, "en", 0)

	require.Len(t, stub.requestBetas(), 2, "the voice turn asks the model")
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role)
	assert.Equal(t, transcript, text, "and the request ends with the transcript")
	assert.Len(t, sent(), 2, "both get a reply (the voice one in text, as speech is off)")
	assert.Equal(t, transcript, storedRow(t, b, chatID, transcript).Text)
}

// R3 inverted: a text collected behind a voice turn whose transcription
// fails is still answered, because the failure notice is aside.
func TestHandleVoiceMessage_TranscriptionFailureLeavesCollectedTextAnswered(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := setupBotForTest(t, 123)
	useAnthropicStub(b, stub)
	sent := recordSends(tg)
	voices := &voiceStubs{release: make(chan struct{})} // no transcript: speech to text fails
	voices.install(t, b, tg)
	const chatID = int64(123)
	voice := voiceMessage(chatID, 907)
	userMsg, err := b.screenIncomingMessage(voice)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleVoiceMessage(context.Background(), voice, userMsg, replyTarget{chatID: chatID},
			123, "owner", "", "", false, "en", 0)
	}()
	require.Eventually(t, func() bool { return !chatIdle(b, chatID) }, 5*time.Second, 5*time.Millisecond,
		"the voice turn is transcribing")
	late := storeUserText(t, b, chatID, "and also, what time do you open?")
	b.respondToChat(textReq(chatID)) // collected behind the voice turn
	close(voices.release)
	<-done
	waitIdle(t, b, chatID)

	require.Len(t, stub.requestBetas(), 1, "the collected text is answered")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "and also, what time do you open?", text)
	assert.NotNil(t, answeredOn(t, b, late.ID))
	require.NotEmpty(t, sent())
	notice := sent()[0].Text
	assert.True(t, storedRow(t, b, chatID, notice).Aside, "the failure notice is aside")
	assert.True(t, storedRow(t, b, chatID, "[Voice message]").Aside, "so is the voice note")
}

// A sticker is invisible to other turns until its own turn puts it in
// memory, and that turn answers it.
func TestRunStickerTurn_StickerIsAnsweredByItsOwnTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	const chatID = int64(779)
	storeUserText(t, b, chatID, "look at this")
	message := &models.Message{
		ID: 908, Chat: models.Chat{ID: chatID}, From: &models.User{ID: chatID, Username: "tester"},
		Sticker: &models.Sticker{FileID: "sticker-file", Emoji: "😀"},
	}
	sticker, err := b.screenIncomingMessage(message) // the sticker's intake
	require.NoError(t, err)

	b.respondToChat(textReq(chatID)) // a text turn that runs before the sticker's
	require.Len(t, stub.requestBetas(), 1)
	_, text := stub.lastMessage(t, 0)
	assert.Equal(t, "look at this", text, "the text turn does not read the sticker")

	b.runStickerTurn(context.Background(), replyTarget{chatID: chatID}, sticker, message)

	require.Len(t, stub.requestBetas(), 2, "the sticker's turn asks the model")
	role, text := stub.lastMessage(t, 1)
	assert.Equal(t, "user", role)
	assert.Equal(t, "Sent a sticker: 😀", text)
	assert.Len(t, tg.sent(), 2)
}

// Aside rows never load into memory: after a restart the conversation is
// what it was, and the message after the last reply is still answered.
func TestReload_SkipsAsideRows(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	const chatID = int64(793)
	base := time.Now().Add(-time.Hour)
	for i, row := range []struct {
		text          string
		isUser, aside bool
	}{
		{"where is the candle farm?", true, false},
		{"/whoami", true, true},
		{"👤 Your Information: ...", false, true},
		{"Open the Farm tab.", false, false},
		{"Rate limit exceeded. Please try again later.", false, true},
		{"and the bakery?", true, false},
	} {
		m := b.createMessage(chatID, 555, "tester", "user", row.text, row.isUser)
		m.Timestamp = base.Add(time.Duration(i) * time.Minute)
		m.Aside = row.aside
		require.NoError(t, b.storeMessage(&m))
	}

	assert.Equal(t, []string{"where is the candle farm?", "Open the Farm tab.", "and the bakery?"}, memoryTexts(b, chatID))
	b.respondToChat(textReq(chatID))
	require.Len(t, stub.requestBetas(), 1, "the message after the last reply is answered")
	role, text := stub.lastMessage(t, 0)
	assert.Equal(t, "user", role)
	assert.Equal(t, "and the bakery?", text)
}

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfHealStub fails its first deadFiles requests with the 404 the Files API
// gives for a file it no longer has, running hook before the first, then
// answers every later request with candleFarmStream.
type selfHealStub struct {
	deadFile  string
	deadFiles int
	hook      func()

	mu     sync.Mutex
	bodies [][]byte
}

func (s *selfHealStub) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if n < s.deadFiles {
			if n == 0 && s.hook != nil {
				s.hook()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"File not found: `+s.deadFile+`"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseFrames(candleFarmStream()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *selfHealStub) body(t *testing.T, i int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Less(t, i, len(s.bodies), "request %d was never made", i)
	return string(s.bodies[i])
}

func (s *selfHealStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// selfHealBot is a bot whose Anthropic client talks to stub.
func selfHealBot(t *testing.T, stub *selfHealStub) *Bot {
	t.Helper()
	b, _ := historyTestBot(t, nil)
	b.anthropicClient = anthropic.NewClient(option.WithBaseURL(stub.serve(t)), option.WithAPIKey("test"), option.WithMaxRetries(0))
	return b
}

// storePhoto puts a user photo whose file the Files API has since dropped
// into the chat, with caption as its text.
func storePhoto(t *testing.T, b *Bot, chatID int64, caption string) Message {
	t.Helper()
	b.getOrCreateChatMemory(chatID)
	m := b.createMessage(chatID, 555, "tester", "user", caption, true)
	m.ImageFileIDs = []string{"file_dead"}
	require.NoError(t, b.storeMessage(&m))
	joined, ok := b.joinConversation(m)
	require.True(t, ok)
	return joined
}

// R4 inverted: the retry after a dead file id sends the request the turn
// built, with the image swapped for a note, not live memory. A message that
// landed meanwhile is not read, so the turn neither answers it nor files its
// reply before it, and its own flush answers it once.
func TestGetAnthropicResponse_SelfHealRetryKeepsTheSnapshot(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, landed := range map[string]func(t *testing.T, b *Bot, chatID int64){
		"a user message": func(t *testing.T, b *Bot, chatID int64) {
			storeUserText(t, b, chatID, "and is it still there?")
		},
		"a bot message": func(_ *testing.T, b *Bot, chatID int64) {
			b.joinConversation(Message{ChatID: chatID, Text: "a bot message filed meanwhile"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &selfHealStub{deadFile: "file_dead", deadFiles: 1}
			b := selfHealBot(t, stub)
			const chatID = int64(783)
			storePhoto(t, b, chatID, "old photo")
			answerInTurn(t, b, chatID, "nice photo")
			storeUserText(t, b, chatID, "what was in that photo?")
			stub.hook = func() { landed(t, b, chatID) }

			b.respondToChat(textReq(chatID)) // 404, self-heal, retry, answer

			require.Equal(t, 2, stub.count())
			retried := stub.body(t, 1)
			role, text := lastRoleText(t, []byte(retried))
			assert.Equal(t, "user", role, "the retry ends with the user's message")
			assert.Equal(t, "what was in that photo?", text, "the one the turn read")
			assert.NotContains(t, retried, "and is it still there?", "a message that landed meanwhile is not read")
			assert.NotContains(t, retried, "a bot message filed meanwhile")
			assert.NotContains(t, retried, "file_dead", "the dead image is gone from the request")
			assert.Contains(t, retried, imageGoneText, "and a note stands in for it")
			for _, m := range b.getOrCreateChatMemory(chatID).Messages {
				assert.NotContains(t, m.ImageFileIDs, "file_dead", "later turns do not send it either")
			}
		})
	}
}

// The landed message's flush answers it once, after the healed turn.
func TestGetAnthropicResponse_SelfHealLeavesLateMessageToItsOwnTurn(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := &selfHealStub{deadFile: "file_dead", deadFiles: 1}
	b := selfHealBot(t, stub)
	const chatID = int64(784)
	storePhoto(t, b, chatID, "old photo")
	answerInTurn(t, b, chatID, "nice photo")
	storeUserText(t, b, chatID, "what was in that photo?")
	stub.hook = func() { storeUserText(t, b, chatID, "and is it still there?") }

	b.respondToChat(textReq(chatID))
	b.respondToChat(textReq(chatID)) // the late message's own flush

	require.Equal(t, 3, stub.count())
	role, text := lastRoleText(t, []byte(stub.body(t, 2)))
	assert.Equal(t, "user", role)
	assert.Equal(t, "and is it still there?", text)
	answered := 0
	for i := 0; i < stub.count(); i++ {
		if r, last := lastRoleText(t, []byte(stub.body(t, i))); r == "user" && last == "and is it still there?" {
			answered++
		}
	}
	assert.Equal(t, 1, answered, "the late message is answered once")
}

// A photo sent without a caption is all its message holds. Its dead file
// becomes a note, so the message keeps content, and its cache breakpoint.
func TestGetAnthropicResponse_SelfHealKeepsAPhotoOnlyMessage(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := &selfHealStub{deadFile: "file_dead", deadFiles: 1}
	b := selfHealBot(t, stub)
	const chatID = int64(794)
	storePhoto(t, b, chatID, "")

	b.respondToChat(textReq(chatID))

	require.Equal(t, 2, stub.count())
	retried := stub.body(t, 1)
	role, text := lastRoleText(t, []byte(retried))
	assert.Equal(t, "user", role)
	assert.Equal(t, imageGoneText, text)
	assert.Contains(t, retried, `"cache_control"`, "the breakpoint moved to the note")
}

// A 404 for a file the request does not hold cannot be healed by retrying.
func TestGetAnthropicResponse_SelfHealGivesUpOnAFileNotInTheRequest(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := &selfHealStub{deadFile: "file_elsewhere", deadFiles: maxFileNotFoundRetries + 1}
	b := selfHealBot(t, stub)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	messages := []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi"))}

	_, err := b.getAnthropicResponse(ctx, 795, messages, false, "alice", "", "", false, "en", 0, replySinks{})

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "file_elsewhere"), err.Error())
	assert.Equal(t, 1, stub.count(), "no retry")
}

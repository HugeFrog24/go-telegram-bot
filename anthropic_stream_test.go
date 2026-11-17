package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notesBetaHeader = "thinking-display-updates-2026-08-18"

// The stream events a fixture is built from. Each is the JSON of one SSE data
// line; sseFrames adds the matching "event:" line.

func jsonString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func evMessageStart() string {
	return `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant",` +
		`"model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":10,"output_tokens":1}}}`
}

func evBlockStart(index int, block string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, block)
}

func evDelta(index int, deltaType, field, value string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":%q,%q:%s}}`,
		index, deltaType, field, jsonString(value))
}

func evBlockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

func evMessageEnd(stopReason string) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":20}}`, stopReason),
		`{"type":"message_stop"}`,
	}
}

// evTextBlock is a whole text block: start, one delta, stop.
func evTextBlock(index int, text string) []string {
	return []string{
		evBlockStart(index, `{"type":"text","text":""}`),
		evDelta(index, "text_delta", "text", text),
		evBlockStop(index),
	}
}

// evThinkingBlock is a progress-update block as display "updates" streams it:
// a thinking_delta with text, then the signature.
func evThinkingBlock(index int, text string) []string {
	return []string{
		evBlockStart(index, `{"type":"thinking","thinking":"","signature":""}`),
		evDelta(index, "thinking_delta", "thinking", text),
		evDelta(index, "signature_delta", "signature", "c2lnbmF0dXJl"),
		evBlockStop(index),
	}
}

func evServerToolBlock(index int) []string {
	return []string{
		evBlockStart(index, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`),
		evDelta(index, "input_json_delta", "partial_json", `{"query":"candle farm"}`),
		evBlockStop(index),
	}
}

func evStream(parts ...[]string) []string {
	events := []string{evMessageStart()}
	for _, p := range parts {
		events = append(events, p...)
	}
	return events
}

// candleFarmStream is the stream the brief describes: a progress note, an MCP
// call and its result, then the answer.
func candleFarmStream() []string {
	return evStream(
		evThinkingBlock(0, "Searching the docs for the candle farm."),
		[]string{
			evBlockStart(1, `{"type":"mcp_tool_use","id":"mcptoolu_1","name":"list_documents","server_name":"tibik-docs","input":{}}`),
			evDelta(1, "input_json_delta", "partial_json", `{"query":"candle farm"}`),
			evBlockStop(1),
			evBlockStart(2, `{"type":"mcp_tool_result","tool_use_id":"mcptoolu_1","is_error":false,"content":[{"type":"text","text":"Farm tab"}]}`),
			evBlockStop(2),
		},
		evTextBlock(3, "Open the Farm tab."),
		evMessageEnd("end_turn"),
	)
}

// sseFrames renders events as the body of a text/event-stream response.
func sseFrames(events []string) string {
	var sb strings.Builder
	for _, data := range events {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &head); err != nil {
			panic(err)
		}
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", head.Type, data)
	}
	return sb.String()
}

// anthropicStub answers every POST to /v1/messages with the next of its
// streams, repeating the last one once they run out.
type anthropicStub struct {
	url     string
	streams [][]string

	// With gate set, each stream stops after its first split events until
	// gate is closed.
	gate  chan struct{}
	split int

	mu      sync.Mutex
	betas   []string // the anthropic-beta header of each request, in order
	bodies  [][]byte // each request's body, in order
	deletes []string // the path of each Files API delete, in order
}

// deletedFiles is the path of every Files API delete the stub saw.
func (s *anthropicStub) deletedFiles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deletes...)
}

func newAnthropicStub(t *testing.T, streams ...[]string) *anthropicStub {
	t.Helper()
	return serveAnthropicStub(t, &anthropicStub{streams: streams})
}

// newGatedAnthropicStub answers with one stream that stops after its first
// split events until open is called, the way a slow tool call holds up the
// real stream. The gate also opens when the test ends.
func newGatedAnthropicStub(t *testing.T, events []string, split int) (stub *anthropicStub, open func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(gate) }) }
	stub = serveAnthropicStub(t, &anthropicStub{streams: [][]string{events}, gate: gate, split: split})
	t.Cleanup(open) // runs before the server's Close, which waits for the handler
	return stub, open
}

func serveAnthropicStub(t *testing.T, stub *anthropicStub) *anthropicStub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v1/files") {
			if r.Method == http.MethodDelete {
				stub.mu.Lock()
				stub.deletes = append(stub.deletes, r.URL.Path)
				stub.mu.Unlock()
			}
			serveFilesStub(w, r)
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		n := len(stub.betas)
		stub.betas = append(stub.betas, r.Header.Get("anthropic-beta"))
		stub.bodies = append(stub.bodies, body)
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		events := stub.streams[min(n, len(stub.streams)-1)]
		if stub.gate != nil {
			_, _ = io.WriteString(w, sseFrames(events[:stub.split]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-stub.gate:
			case <-r.Context().Done():
				return
			}
			events = events[stub.split:]
		}
		_, _ = io.WriteString(w, sseFrames(events))
	}))
	t.Cleanup(srv.Close)
	stub.url = srv.URL
	return stub
}

// serveFilesStub answers the Files API calls a photo turn makes: every
// upload becomes file_stub, and every delete succeeds.
func serveFilesStub(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodPost:
		_, _ = io.WriteString(w, `{"id":"file_stub","type":"file","filename":"photo.jpg","mime_type":"image/jpeg",`+
			`"size_bytes":4,"created_at":"2026-11-16T00:00:00Z","downloadable":false}`)
	case http.MethodDelete:
		_, _ = io.WriteString(w, `{"id":"file_stub","type":"file_deleted"}`)
	default:
		http.NotFound(w, r)
	}
}

// body is request i's body. It fails the test when the stub never saw
// request i.
func (s *anthropicStub) body(t *testing.T, i int) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.bodies) {
		t.Fatalf("request %d was never made: the stub saw %d", i, len(s.bodies))
	}
	return s.bodies[i]
}

// lastMessage is the role and text of the final entry in request i's
// messages, the turn the model is asked to answer.
func (s *anthropicStub) lastMessage(t *testing.T, i int) (role, text string) {
	t.Helper()
	return lastRoleText(t, s.body(t, i))
}

// lastRoleText is the role and text of the final entry in a request body's
// messages.
func lastRoleText(t *testing.T, body []byte) (role, text string) {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		t.Fatalf("the request has no messages: %v", err)
	}
	last := req.Messages[len(req.Messages)-1]
	var sb strings.Builder
	for _, c := range last.Content {
		sb.WriteString(c.Text)
	}
	return last.Role, sb.String()
}

func (s *anthropicStub) requestBetas() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.betas...)
}

// streamTestBot is a bot whose Anthropic client talks to stub, with progress
// notes switched on or off the way the config does it.
func streamTestBot(t *testing.T, stub *anthropicStub, notesOn bool) *Bot {
	t.Helper()
	b, _ := setupBotForTest(t, 123)
	if notesOn {
		b.config.Thinking = ThinkingModeAdaptive
		b.config.ThinkingDisplay = ThinkingDisplayUpdates
	}
	useAnthropicStub(b, stub)
	return b
}

// useAnthropicStub points b's Anthropic client at stub, never the real API.
func useAnthropicStub(b *Bot, stub *anthropicStub) {
	b.anthropicClient = anthropic.NewClient(
		option.WithBaseURL(stub.url),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
}

func askStubbedModel(t *testing.T, b *Bot, sinks replySinks) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	messages := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("Where is the candle farm?")),
	}
	return b.getAnthropicResponse(ctx, chatKey(42), messages, false, "alice", "Alice", "", false, "en", 0, sinks)
}

func TestGetAnthropicResponse_StreamNotesAndAnswer(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	require.NoError(t, err)
	assert.Equal(t, []string{"Searching the docs for the candle farm."}, rec.notes)
	assert.Equal(t, []string{"Open the Farm tab."}, rec.segments)
	assert.Equal(t, "Open the Farm tab.", reply)
	assert.Equal(t, "Open the Farm tab.", rec.lastProgress())

	betas := stub.requestBetas()
	require.Len(t, betas, 1, "the request must reach the stub, never the real API")
	assert.Contains(t, betas[0], notesBetaHeader)
}

func TestGetAnthropicResponse_StreamNotesOff(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b := streamTestBot(t, stub, false)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(false))

	require.NoError(t, err)
	assert.Empty(t, rec.notes)
	assert.Equal(t, []string{"Open the Farm tab."}, rec.segments)
	assert.Equal(t, "Open the Farm tab.", reply)
	assert.Equal(t, "Open the Farm tab.", rec.lastProgress())

	betas := stub.requestBetas()
	require.Len(t, betas, 1)
	assert.NotContains(t, betas[0], notesBetaHeader)
}

func TestGetAnthropicResponse_StreamShortTextBeforeToolIsNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(
		evTextBlock(0, "Let me search the web."),
		evServerToolBlock(1),
		evTextBlock(2, "The candle farm is on the Farm tab."),
		evMessageEnd("end_turn"),
	))
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	require.NoError(t, err)
	assert.Equal(t, []string{"Let me search the web."}, rec.notes)
	assert.Equal(t, []string{"The candle farm is on the Farm tab."}, rec.segments)
	assert.Equal(t, "The candle farm is on the Farm tab.", reply)
	assert.Contains(t, rec.progress, "", "the draft dropped the text that became a note")
}

func TestGetAnthropicResponse_StreamPauseTurnKeepsHeldText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t,
		evStream(evTextBlock(0, "Let me look."), evMessageEnd("pause_turn")),
		evStream(evServerToolBlock(0), evTextBlock(1, "Found it."), evMessageEnd("end_turn")),
	)
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	require.NoError(t, err)
	assert.Len(t, stub.requestBetas(), 2, "pause_turn continues the turn with a second request")
	assert.Equal(t, []string{"Let me look."}, rec.notes, "the held text carried over the pause and the tool call made it a note")
	assert.Equal(t, []string{"Found it."}, rec.segments)
	assert.Equal(t, "Found it.", reply)
}

func TestGetAnthropicResponse_StreamPauseTurnLimitCommitsHeldText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(evTextBlock(0, "Let me look."), evMessageEnd("pause_turn")))
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	require.NoError(t, err)
	assert.Len(t, stub.requestBetas(), maxPauseTurnContinuations+1)
	assert.Empty(t, rec.notes)
	want := strings.TrimSuffix(strings.Repeat("Let me look.\n\n", maxPauseTurnContinuations+1), "\n\n")
	assert.Equal(t, want, reply, "giving up on the turn returns the partial answer, held text included")
}

func TestGetAnthropicResponse_StreamOnlyNotesIsEmptyReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(
		evThinkingBlock(0, "Looking into it."),
		evMessageEnd("end_turn"),
	))
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	assert.Equal(t, []string{"Looking into it."}, rec.notes)
	assert.Equal(t, "", reply)
	assert.EqualError(t, err, emptyStreamError("end_turn", "", 0, defaultMaxTokens).Error())
}

func TestGetAnthropicResponse_StreamPlaceholderNeverShown(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, evStream(
		evTextBlock(0, "Here is the answer, long enough to stand alone."+strings.Repeat(" ok", 100)),
		evThinkingBlock(1, interruptedNotePlaceholder),
		evMessageEnd("max_tokens"),
	))
	b := streamTestBot(t, stub, true)
	rec := &sinkRecorder{}

	reply, err := askStubbedModel(t, b, rec.sinks(true))

	require.NoError(t, err)
	assert.Empty(t, rec.notes)
	assert.NotContains(t, reply, interruptedNotePlaceholder)
	assert.Len(t, rec.segments, 1)
}

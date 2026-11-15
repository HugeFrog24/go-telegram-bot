package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/stretchr/testify/assert"
)

// sinkRecorder collects everything an assembler sends to its sinks, in order.
type sinkRecorder struct {
	segments, notes, progress []string
	segmentErr                error
}

// sinks returns the recorder's sinks. With notesOn false onNote stays nil, the
// way every caller passes it before Task 6 wires notes in.
func (r *sinkRecorder) sinks(notesOn bool) replySinks {
	s := replySinks{
		onSegment: func(text string) error {
			r.segments = append(r.segments, text)
			return r.segmentErr
		},
		onProgress: func(text string) { r.progress = append(r.progress, text) },
	}
	if notesOn {
		s.onNote = func(text string) { r.notes = append(r.notes, text) }
	}
	return s
}

func (r *sinkRecorder) lastProgress() string {
	if len(r.progress) == 0 {
		return "<no progress call>"
	}
	return r.progress[len(r.progress)-1]
}

func newTestAssembler(notesOn bool) (*replyAssembler, *sinkRecorder) {
	rec := &sinkRecorder{}
	return newReplyAssembler(rec.sinks(notesOn)), rec
}

func TestReplyAssembler_NotesOffMatchesToday(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(false)

	a.blockStart("text")
	a.textDone(" Hi ")
	assert.Equal(t, []string{"Hi"}, rec.segments)

	a.blockStart("thinking")
	a.thinkingDone("Checking")
	assert.Empty(t, rec.notes)

	a.blockStart("mcp_tool_use")
	a.blockStart("text")
	a.textDone("Short.")
	assert.Equal(t, []string{"Hi", "Short."}, rec.segments, "short text is committed at once with notes off")

	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Equal(t, []string{"Hi", "Short."}, rec.segments)
	assert.Equal(t, "Hi\n\nShort.", a.reply())
	assert.Empty(t, rec.progress, "blockStart and responseEnd never redraw with notes off")
}

func TestReplyAssembler_NotesOffEmptyTextIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(false)
	a.textDone("  \n ")
	assert.Empty(t, rec.segments)
	assert.Equal(t, "", a.reply())
}

func TestReplyAssembler_NotesOffSegmentErrorKeepsReply(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	var logged bytes.Buffer
	prev := ErrorLogger
	ErrorLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { ErrorLogger = prev })

	a, rec := newTestAssembler(false)
	rec.segmentErr = errors.New("telegram is down")
	a.textDone("Hi")

	assert.Equal(t, "Hi", a.reply(), "a failed onSegment does not lose the text")
	assert.Contains(t, logged.String(), "[stream] onSegment failed: telegram is down")
}

func TestReplyAssembler_NilSinksAreSafe(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, sinks := range []replySinks{{}, {onNote: func(string) {}}} {
		assert.NotPanics(t, func() {
			a := newReplyAssembler(sinks)
			a.blockStart("tool_use")
			a.thinkingDone("Checking")
			a.blockStart("text")
			a.progress("Hi there")
			a.textDone("Hi")
			a.blockStart("tool_use")
			a.responseEnd(anthropic.BetaStopReasonEndTurn)
		})
	}
}

func TestReplyAssembler_ProgressShowsReplySoFar(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.progress("Hel")
	assert.Equal(t, "Hel", rec.lastProgress())

	a.textDone("Hello.") // held: a short text block not yet known to be an answer
	a.progress("Next")
	assert.Equal(t, "Hello.\n\nNext", rec.lastProgress(), "the held text is part of the draft until it is decided")

	a.blockStart("text") // the next block is text, so the held text is the answer
	a.progress("Next")
	assert.Equal(t, "Hello.\n\nNext", rec.lastProgress())
}

func TestReplyAssembler_ThinkingNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.blockStart("thinking")
	a.thinkingDone(" Checking the docs ")
	assert.Equal(t, []string{"Checking the docs"}, rec.notes)
	assert.Equal(t, "", a.reply(), "a note is never part of the reply")
	assert.Empty(t, rec.segments)
}

func TestReplyAssembler_ThinkingNoteLogged(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })

	a, rec := newTestAssembler(true)
	a.thinkingDone("Checking the docs")
	assert.Contains(t, logged.String(), "[note] Checking the docs")

	logged.Reset()
	long := strings.Repeat("ж", 600)
	a.thinkingDone(long)
	assert.Equal(t, []string{"Checking the docs", long}, rec.notes, "the sink gets the whole note")
	assert.Contains(t, logged.String(), "[note] "+strings.Repeat("ж", 500)+"...(truncated)")
	assert.NotContains(t, logged.String(), strings.Repeat("ж", 501))
}

func TestReplyAssembler_EmptyThinkingIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.thinkingDone("  ")
	a.thinkingDone("")
	assert.Empty(t, rec.notes)
}

func TestReplyAssembler_PlaceholderNeverShown(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.thinkingDone(interruptedNotePlaceholder)
	a.thinkingDone("  " + interruptedNotePlaceholder + "\n")
	a.textDone(interruptedNotePlaceholder)
	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Empty(t, rec.notes)
	assert.Empty(t, rec.segments)
	assert.Equal(t, "", a.reply())
}

func TestReplyAssembler_EmptyTextDropped(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("   ")
	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Empty(t, rec.segments)
	assert.Empty(t, rec.notes)
	assert.Equal(t, "", a.reply())
}

func TestReplyAssembler_ShortTextBeforeToolIsNote(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, kind := range []string{"tool_use", "server_tool_use", "mcp_tool_use"} {
		t.Run(kind, func(t *testing.T) {
			a, rec := newTestAssembler(true)
			a.textDone("Let me check the docs.")
			assert.Empty(t, rec.segments, "a short text block waits for the next block")

			a.blockStart(kind)
			assert.Equal(t, []string{"Let me check the docs."}, rec.notes)
			assert.Empty(t, rec.segments)
			assert.Equal(t, "", a.reply())
			assert.Equal(t, "", rec.lastProgress(), "the draft drops the text that became a note")

			a.responseEnd(anthropic.BetaStopReasonEndTurn)
			assert.Empty(t, rec.segments, "the note is not committed again at the end")
			assert.Equal(t, "", a.reply())
		})
	}
}

func TestReplyAssembler_NoteDropsOnlyHeldTextFromDraft(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("First answer is long enough to commit. " + strings.Repeat("x", maxTextNoteRunes))
	a.textDone("Let me check.")
	a.blockStart("mcp_tool_use")
	assert.Equal(t, []string{"Let me check."}, rec.notes)
	assert.Equal(t, a.reply(), rec.lastProgress(), "the draft falls back to the committed answer")
	assert.NotEmpty(t, a.reply())
}

func TestReplyAssembler_ShortTextBeforeOtherBlockIsAnswer(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for _, kind := range []string{"text", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			a, rec := newTestAssembler(true)
			a.textDone("Sure.")
			assert.Empty(t, rec.segments)

			a.blockStart(kind)
			assert.Equal(t, []string{"Sure."}, rec.segments)
			assert.Empty(t, rec.notes)
			assert.Equal(t, "Sure.", a.reply())
		})
	}
}

func TestReplyAssembler_LongTextBeforeToolIsAnswer(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone(strings.Repeat("a", maxTextNoteRunes+1))
	assert.Equal(t, []string{strings.Repeat("a", maxTextNoteRunes+1)}, rec.segments, "over the limit, committed right after textDone")

	a.blockStart("mcp_tool_use")
	assert.Empty(t, rec.notes)
	assert.Equal(t, []string{strings.Repeat("a", maxTextNoteRunes+1)}, rec.segments)
}

func TestReplyAssembler_RuneBoundary(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	atLimit := strings.Repeat("ж", maxTextNoteRunes) // 560 bytes, 280 runes
	assert.Greater(t, len(atLimit), maxTextNoteRunes)

	a, rec := newTestAssembler(true)
	a.textDone(atLimit)
	a.blockStart("tool_use")
	assert.Equal(t, []string{atLimit}, rec.notes, "280 runes still count as a note")
	assert.Empty(t, rec.segments)

	b, rec2 := newTestAssembler(true)
	b.textDone(atLimit + "ж")
	assert.Equal(t, []string{atLimit + "ж"}, rec2.segments, "281 runes are an answer")
	b.blockStart("tool_use")
	assert.Empty(t, rec2.notes)
}

func TestReplyAssembler_LimitAppliesAfterTrimming(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("  " + strings.Repeat("ж", maxTextNoteRunes) + "\n\n")
	a.blockStart("tool_use")
	assert.Len(t, rec.notes, 1)
	assert.Empty(t, rec.segments)
}

func TestReplyAssembler_HeldTextFlushedAtEnd(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("All done.")
	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Equal(t, []string{"All done."}, rec.segments)
	assert.Empty(t, rec.notes)
	assert.Equal(t, "All done.", a.reply())
}

func TestReplyAssembler_PauseTurnKeepsHeldText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("Let me look.")
	a.responseEnd(anthropic.BetaStopReasonPauseTurn)
	assert.Empty(t, rec.segments)
	assert.Empty(t, rec.notes)
	assert.Equal(t, "", a.reply())

	a.blockStart("server_tool_use") // the continuation opens with the tool call
	assert.Equal(t, []string{"Let me look."}, rec.notes)
	assert.Empty(t, rec.segments)
}

func TestReplyAssembler_AccumulatesAcrossResponses(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("Let me look.")
	a.responseEnd(anthropic.BetaStopReasonPauseTurn)
	a.blockStart("text") // the continuation opens with text, so the held text was an answer
	a.textDone("Found it.")
	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Equal(t, []string{"Let me look.", "Found it."}, rec.segments)
	assert.Equal(t, "Let me look.\n\nFound it.", a.reply())
}

func TestReplyAssembler_NewTextFlushesUndecidedHeldText(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	a, rec := newTestAssembler(true)
	a.textDone("One.")
	a.textDone("Two.") // blockStart was skipped; the first text must not be lost
	a.responseEnd(anthropic.BetaStopReasonEndTurn)
	assert.Equal(t, []string{"One.", "Two."}, rec.segments)
	assert.Equal(t, "One.\n\nTwo.", a.reply())
}

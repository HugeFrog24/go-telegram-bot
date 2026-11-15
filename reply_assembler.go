package main

import (
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

const (
	// maxTextNoteRunes is the longest text block that can still be a progress
	// note: one immediately followed by a tool call and no longer than this,
	// after trimming. Anything longer is the answer.
	maxTextNoteRunes = 280
	// interruptedNotePlaceholder is what the API writes into a progress-update
	// block when the response stopped before the model finished it. It says
	// nothing the user needs, so it is never shown.
	interruptedNotePlaceholder = "This part of the response was interrupted before it finished."
	// maxNoteLogRunes caps how much of a note the [note] log line prints.
	maxNoteLogRunes = 500
)

// replySinks is where one turn's reply goes while the model produces it.
// Every field may be nil.
type replySinks struct {
	onSegment  func(string) error // an answer text block is final
	onProgress func(string)       // the answer so far, for in-place redraws
	onNote     func(string)       // a progress note; nil turns notes off for the turn
}

// replyAssembler decides, block by block, what in the model's stream is answer
// text and what is a progress note, and sends each to its sink. One assembler
// serves one turn, across every request the turn makes (pause_turn
// continuations, file self-heal retries). It is used from the single goroutine
// that reads the stream and is not safe for concurrent use.
//
// With onNote nil it behaves as the code did before notes existed: every
// non-empty text block is an answer segment the moment it finishes.
//
// With onNote set a thinking block with text is a note, and so is a short text
// block that a tool call follows at once. The model's next block is the only
// way to tell the two apart, so a short text block is held until then.
type replyAssembler struct {
	sinks  replySinks
	answer string // committed answer text, joined with joinNonEmpty
	held   string // a short text block that may yet turn out to be a note
}

func newReplyAssembler(s replySinks) *replyAssembler {
	return &replyAssembler{sinks: s}
}

func (a *replyAssembler) notesOn() bool { return a.sinks.onNote != nil }

// blockStart is called when a content block of the given type opens, before
// anything else is done with that block. It settles the held text: a tool call
// right after it makes it a note, any other block makes it part of the answer.
func (a *replyAssembler) blockStart(kind string) {
	if a.held == "" {
		return
	}
	switch kind {
	case "tool_use", "server_tool_use", "mcp_tool_use":
		note := a.held
		a.held = ""
		a.emitNote(note)
		// The draft was showing the text as it streamed in; redraw without it.
		if a.sinks.onProgress != nil {
			a.sinks.onProgress(a.answer)
		}
	default:
		a.flushHeld()
	}
}

// progress reports the text block being streamed: onProgress gets the whole
// answer so far, held text included, followed by the block's text so far.
func (a *replyAssembler) progress(currentText string) {
	if a.sinks.onProgress != nil {
		a.sinks.onProgress(joinNonEmpty(joinNonEmpty(a.answer, a.held), currentText))
	}
}

// textDone is called with the raw text of a finished text block.
func (a *replyAssembler) textDone(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if a.notesOn() {
		if text == interruptedNotePlaceholder {
			return
		}
		a.flushHeld() // blockStart settles this first; keep order if it was skipped
		if utf8.RuneCountInString(text) <= maxTextNoteRunes {
			a.held = text
			return
		}
	}
	a.commit(text)
}

// thinkingDone is called with the raw text of a finished thinking block. Under
// display "updates" a thinking block carries text only when it is a progress
// note; reasoning blocks stay empty.
func (a *replyAssembler) thinkingDone(text string) {
	if !a.notesOn() {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" || text == interruptedNotePlaceholder {
		return
	}
	a.emitNote(text)
}

// responseEnd is called when one request's stream has ended cleanly. After a
// pause_turn the model carries on in the next request, whose first block still
// decides what the held text was; any other stop ends the turn.
func (a *replyAssembler) responseEnd(stop anthropic.BetaStopReason) {
	if stop == anthropic.BetaStopReasonPauseTurn {
		return
	}
	a.flushHeld()
}

// reply is the answer text committed so far.
func (a *replyAssembler) reply() string { return a.answer }

// flushHeld commits the held text as part of the answer.
func (a *replyAssembler) flushHeld() {
	if a.held == "" {
		return
	}
	text := a.held
	a.held = ""
	a.commit(text)
}

func (a *replyAssembler) commit(text string) {
	a.answer = joinNonEmpty(a.answer, text)
	if a.sinks.onSegment == nil {
		return
	}
	if err := a.sinks.onSegment(text); err != nil {
		ErrorLogger.Printf("[stream] onSegment failed: %v", err)
	}
}

// emitNote logs a progress note and hands it to the note sink.
func (a *replyAssembler) emitNote(text string) {
	preview := clampRunes(text, maxNoteLogRunes)
	if len(preview) < len(text) {
		preview += "...(truncated)"
	}
	InfoLogger.Printf("[note] %s", preview)
	a.sinks.onNote(text)
}

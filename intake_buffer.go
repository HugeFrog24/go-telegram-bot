package main

import (
	"context"
	"time"
)

// pendingIntake holds a chat's coalescing window. The buffered message bodies
// are deliberately absent: each message is stored and has joined chat memory
// by the time it is buffered, so the flushed turn picks them all up from
// memory. What is kept here is the request
// the turn needs, always refreshed to the most recent message in the batch.
type pendingIntake struct {
	turnRequest
	allEmojiOnly bool // every message in the batch was emoji only
	count        int
	seq          uint64
	timer        *time.Timer
}

// bufferIntake holds a text message for the configured quiet window instead of
// dispatching a turn immediately, resetting the window on each new message.
// Rapid follow-ups therefore produce one reply rather than one per message.
// req is the request for the message being buffered; its isEmojiOnly feeds the
// batch's flag, and flushIntake sets the flushed request's own.
func (b *Bot) bufferIntake(req turnRequest) {
	window := b.config.DebounceWindow()

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()

	pending, exists := b.intakeBuffers[req.chatID]
	if !exists {
		b.intakeSeq++
		pending = &pendingIntake{seq: b.intakeSeq, allEmojiOnly: true}
		b.intakeBuffers[req.chatID] = pending
	}

	// Reply metadata tracks the most recent message in the batch.
	pending.allEmojiOnly = pending.allEmojiOnly && req.isEmojiOnly
	pending.turnRequest = req
	pending.count++

	b.armIntakeLocked(pending, window)
}

// armIntakeLocked starts or restarts the quiet-window timer. The caller holds
// intakeBuffersMu.
func (b *Bot) armIntakeLocked(pending *pendingIntake, window time.Duration) {
	if pending.timer != nil {
		pending.timer.Stop()
	}
	ctx, chatID, seq := pending.ctx, pending.chatID, pending.seq
	pending.timer = time.AfterFunc(window, func() {
		b.flushIntake(ctx, chatID, seq)
	})
}

// touchIntake restarts the quiet window for a chat with a pending batch, as an
// edit to one of its messages does, and reports whether there was one.
// isEmojiOnly is the edited text's classification; an edit can only make the
// batch less emoji-only, never more, since other messages are not re-read.
func (b *Bot) touchIntake(chatID int64, isEmojiOnly bool) bool {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()

	pending, exists := b.intakeBuffers[chatID]
	if !exists {
		return false
	}
	pending.allEmojiOnly = pending.allEmojiOnly && isEmojiOnly
	b.armIntakeLocked(pending, b.config.DebounceWindow())
	return true
}

// flushIntake dispatches the coalesced turn for a chat. seq guards against a
// timer that had already fired before its Stop call landed: a stale goroutine
// would otherwise flush a buffer belonging to a later batch.
func (b *Bot) flushIntake(ctx context.Context, chatID int64, seq uint64) {
	b.intakeBuffersMu.Lock()
	pending, exists := b.intakeBuffers[chatID]
	if !exists || pending.seq != seq {
		b.intakeBuffersMu.Unlock()
		return
	}
	delete(b.intakeBuffers, chatID)
	captured := *pending
	b.intakeBuffersMu.Unlock()

	if captured.count > 1 {
		InfoLogger.Printf("[%s] intake flush: coalesced %d messages into one turn for chat %d",
			b.config.ID, captured.count, chatID)
	}

	req := captured.request()
	req.ctx = ctx
	b.respondToChat(req)
}

// request is the turn the batch's flush runs.
func (p *pendingIntake) request() turnRequest {
	req := p.turnRequest
	req.isEmojiOnly = p.allEmojiOnly
	return req
}

// cancelIntake drops a chat's pending buffer without dispatching, returning how
// many messages were discarded.
//
// This is the fix for the class of bug in openclaw/openclaw#51046, where a stop
// command aborted the running turn but left the debounce buffer armed, so the
// timer fired afterwards and started the very turn the user had just cancelled.
// Here the stakes are higher than a stray turn: /clear and /clear_hard delete
// chat memory, and a surviving buffer would repopulate it moments later with
// content the user asked to have removed.
func (b *Bot) cancelIntake(chatID int64) int {
	if pending := b.removeIntake(chatID); pending != nil {
		return pending.count
	}
	return 0
}

// takeIntake drops a chat's pending buffer like cancelIntake, for a media
// message whose own turn will read and answer the batch, and hands back the
// request the flush would have run, or nil. The media path passes it to
// answerPending if it ends without a model turn.
func (b *Bot) takeIntake(chatID int64) *turnRequest {
	pending := b.removeIntake(chatID)
	if pending == nil {
		return nil
	}
	req := pending.request()
	return &req
}

// removeIntake stops and removes a chat's pending buffer, returning it, or nil.
func (b *Bot) removeIntake(chatID int64) *pendingIntake {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()

	pending, exists := b.intakeBuffers[chatID]
	if !exists {
		return nil
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	delete(b.intakeBuffers, chatID)
	return pending
}

// answerPending runs, as a text turn of its own, the batch a media path took
// over (takeIntake) and then ended without a model turn to answer it: a voice
// note refused or not transcribed, a photo rate-limited, not uploaded or not
// stored. Called inside the media path's turn, it is collected behind that
// turn and runs once it ends.
func (b *Bot) answerPending(pending *turnRequest) {
	if pending != nil {
		b.respondToChat(*pending)
	}
}

// hasPendingIntake reports whether a chat currently holds a buffered batch.
func (b *Bot) hasPendingIntake(chatID int64) bool {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	_, exists := b.intakeBuffers[chatID]
	return exists
}

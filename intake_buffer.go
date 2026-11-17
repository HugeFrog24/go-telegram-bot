package main

import (
	"context"
	"time"
)

// pendingIntake holds a conversation's coalescing window. The buffered message bodies
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

	// firstSeq and lastSeq are the memory sequences of the batch's first and
	// newest messages. from is who sent the first, and where; mixed is set
	// once a message from anyone else, or from another thread or connection,
	// joins the batch. A media turn takes over only an unmixed batch from its
	// own sender and thread (beginMediaTurn).
	firstSeq, lastSeq uint64
	from              sender
	mixed             bool

	// opened is when the batch's first message was buffered: its quiet
	// window is never pushed past maxBatchWindows windows from then.
	opened time.Time
}

// maxBatchWindows bounds a batch's wait (B6): however often new messages or
// edits re-arm its quiet window, it flushes at most this many windows after
// its first message.
const maxBatchWindows = 4

// bufferIntake holds a text message for the configured quiet window instead of
// dispatching a turn immediately, resetting the window on each new message.
// Rapid follow-ups therefore produce one reply rather than one per message.
// req is the request for the message being buffered; its isEmojiOnly feeds the
// batch's flag, and flushIntake sets the flushed request's own.
func (b *Bot) bufferIntake(req turnRequest) {
	window := b.config.DebounceWindow()

	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()

	k := req.conv()
	pending, exists := b.intakeBuffers[k]
	if !exists {
		b.intakeSeq++
		pending = &pendingIntake{seq: b.intakeSeq, allEmojiOnly: true, firstSeq: req.joinedSeq, from: req.sender(), opened: time.Now()}
		b.intakeBuffers[k] = pending
	}
	pending.mixed = pending.mixed || req.sender() != pending.from
	pending.lastSeq = max(pending.lastSeq, req.joinedSeq)

	// Reply metadata tracks the most recent message in the batch.
	pending.allEmojiOnly = pending.allEmojiOnly && req.isEmojiOnly
	pending.turnRequest = req
	pending.count++

	b.armIntakeLocked(pending, window)
	b.landTextLocked(k, req.joinedSeq) // on record now, in the same step
}

// textsOnWay is a conversation's text messages that joined memory but whose
// batch or turn is not on record yet: a media turn waits for them (G5).
type textsOnWay struct {
	seqs    map[uint64]struct{}
	changed chan struct{} // closed when one lands; nil while no one waits
}

// markTextOnWayLocked records the text message joining with seq. The caller
// holds intakeBuffersMu.
func (b *Bot) markTextOnWayLocked(k convKey, seq uint64) {
	if b.textsOnWay == nil {
		b.textsOnWay = make(map[convKey]*textsOnWay)
	}
	onWay := b.textsOnWay[k]
	if onWay == nil {
		onWay = &textsOnWay{seqs: make(map[uint64]struct{})}
		b.textsOnWay[k] = onWay
	}
	onWay.seqs[seq] = struct{}{}
}

// landTextLocked records that the text message with seq is on record: in a
// batch, a registered or collected turn, or given up. It wakes the media
// turns waiting for it. Landing twice is a no-op. The caller holds
// intakeBuffersMu.
func (b *Bot) landTextLocked(k convKey, seq uint64) {
	onWay := b.textsOnWay[k]
	if onWay == nil {
		return
	}
	if _, ok := onWay.seqs[seq]; !ok {
		return
	}
	delete(onWay.seqs, seq)
	if onWay.changed != nil {
		close(onWay.changed)
		onWay.changed = nil
	}
	if len(onWay.seqs) == 0 {
		delete(b.textsOnWay, k)
	}
}

// landText is landTextLocked taking intakeBuffersMu.
func (b *Bot) landText(k convKey, seq uint64) {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	b.landTextLocked(k, seq)
}

// textOnWayBeforeLocked reports whether a text message that joined before
// ticket is on its way, and if so returns a channel closed when one lands.
// The caller holds intakeBuffersMu.
func (b *Bot) textOnWayBeforeLocked(k convKey, ticket uint64) <-chan struct{} {
	onWay := b.textsOnWay[k]
	if onWay == nil {
		return nil
	}
	for seq := range onWay.seqs {
		if seq < ticket {
			if onWay.changed == nil {
				onWay.changed = make(chan struct{})
			}
			return onWay.changed
		}
	}
	return nil
}

// armIntakeLocked starts or restarts the quiet-window timer, never past
// maxBatchWindows windows from the batch's first message. The caller holds
// intakeBuffersMu.
func (b *Bot) armIntakeLocked(pending *pendingIntake, window time.Duration) {
	if pending.timer != nil {
		pending.timer.Stop()
	}
	delay := window
	if !pending.opened.IsZero() {
		delay = max(min(delay, time.Until(pending.opened.Add(maxBatchWindows*window))), 0)
	}
	ctx, k, seq := pending.ctx, pending.conv(), pending.seq
	pending.timer = time.AfterFunc(delay, func() {
		defer b.recoverPanic("intake flush")
		b.flushIntake(ctx, k, seq)
	})
}

// touchIntake restarts the quiet window for a conversation with a pending
// batch, as an edit to one of its messages does, and reports whether there
// was one.
// isEmojiOnly is the edited text's classification; an edit can only make the
// batch less emoji-only, never more, since other messages are not re-read.
func (b *Bot) touchIntake(k convKey, isEmojiOnly bool) bool {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()

	pending, exists := b.intakeBuffers[k]
	if !exists {
		return false
	}
	pending.allEmojiOnly = pending.allEmojiOnly && isEmojiOnly
	b.armIntakeLocked(pending, b.config.DebounceWindow())
	return true
}

// flushIntake dispatches the coalesced turn for a conversation. seq guards against a
// timer that had already fired before its Stop call landed: a stale goroutine
// would otherwise flush a buffer belonging to a later batch.
//
// The batch leaves the buffer and its turn is registered, or collected behind
// the running turn, in one step under turnsMu, then intakeBuffersMu: a media
// turn never finds the chat free with the batch out of the buffer and not yet
// on record (G5).
func (b *Bot) flushIntake(ctx context.Context, k convKey, seq uint64) {
	want, memoryGen := b.flushWant(k)
	chatMemory := b.getOrCreateChatMemory(k)
	started := func() batchStart {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		return b.startBatchLocked(k, func(p *pendingIntake) bool { return p.seq == seq }, chatMemory, want, memoryGen)
	}()
	if started.turn != nil {
		started.req.ctx = ctx
		b.runTextTurn(started.turn, started.ctx, started.req)
	}
}

// batchStart is a batch's text turn, registered for its caller to run, or
// none (turn nil): no batch was taken, or its flush was dropped or collected.
type batchStart struct {
	req  turnRequest
	turn *chatTurn
	ctx  context.Context
}

// startBatchLocked takes the chat's waiting batch when take accepts it, and
// registers its text turn or collects it behind the running turn
// (beginFlushLocked): the flush's one step, and the text a media turn runs
// ahead of itself (runAheadLocked). want and memoryGen are the chat's flush
// want, read before the lock. The caller holds turnsMu.
func (b *Bot) startBatchLocked(k convKey, take func(*pendingIntake) bool, chatMemory *ChatMemory, want, memoryGen uint64) (started batchStart) {
	captured := b.takeBatchIf(k, take)
	if captured == nil {
		return started
	}
	if captured.count > 1 {
		InfoLogger.Printf("[%s] intake flush: coalesced %d messages into one turn for chat %v",
			b.config.ID, captured.count, k)
	}
	started.req = captured.request()
	started.req.memoryGen = memoryGen
	started.turn, started.ctx, _ = b.beginFlushLocked(started.req, max(want, captured.lastSeq), chatMemory)
	return started
}

// takeBatchIf removes the chat's waiting batch and returns it when take
// accepts it, under intakeBuffersMu.
func (b *Bot) takeBatchIf(k convKey, take func(*pendingIntake) bool) *pendingIntake {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	if pending := b.intakeBuffers[k]; pending == nil || !take(pending) {
		return nil
	}
	return b.removeIntakeLocked(k)
}

// request is the turn the batch's flush runs.
func (p *pendingIntake) request() turnRequest {
	req := p.turnRequest
	req.isEmojiOnly = p.allEmojiOnly
	req.firstSeq = p.firstSeq
	return req
}

// rebuffer puts back a batch a media turn took over but did not fold, or
// whose media did not join: it waits in its chat's quiet window again, merged
// into any batch that started since, and runs as its own text turn.
func (b *Bot) rebuffer(p *pendingIntake) {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	cur, exists := b.intakeBuffers[p.conv()]
	if !exists {
		b.intakeBuffers[p.conv()] = p
		b.armIntakeLocked(p, b.config.DebounceWindow())
		return
	}
	cur.mixed = cur.mixed || p.mixed || cur.from != p.from
	if p.firstSeq != 0 && (cur.firstSeq == 0 || p.firstSeq < cur.firstSeq) {
		cur.firstSeq, cur.from = p.firstSeq, p.from
	}
	if !p.opened.IsZero() && (cur.opened.IsZero() || p.opened.Before(cur.opened)) {
		cur.opened = p.opened // the merged batch's first message is the earlier
	}
	cur.lastSeq = max(cur.lastSeq, p.lastSeq)
	cur.count += p.count
	cur.allEmojiOnly = cur.allEmojiOnly && p.allEmojiOnly
}

// removeIntakeLocked stops and removes a conversation's pending buffer,
// returning it, or nil. The caller holds intakeBuffersMu.
func (b *Bot) removeIntakeLocked(k convKey) *pendingIntake {
	pending, exists := b.intakeBuffers[k]
	if !exists {
		return nil
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	delete(b.intakeBuffers, k)
	return pending
}

// hasPendingIntake reports whether a conversation currently holds a buffered
// batch.
func (b *Bot) hasPendingIntake(k convKey) bool {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	_, exists := b.intakeBuffers[k]
	return exists
}

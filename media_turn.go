package main

import (
	"context"
	"slices"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// mediaWaiter is a media turn waiting for its conversation (beginMediaTurn).
type mediaWaiter struct {
	// ticket is the memory sequence reserved when the media was ready: every
	// message that joined before has a lower one, every later one a higher.
	ticket  uint64
	from    sender
	to      replyTarget // where its message came from, and its reply goes
	row     uint        // its message's row id
	parent  context.Context
	granted chan mediaTurn // closeTurn hands it the chat here
}

// folds reports whether the media turn takes over the waiting batch p: only
// text from its own sender, in its own thread and connection, and no one
// else's (controller decision F2: one turn, one reply that read both).
func (w *mediaWaiter) folds(p *pendingIntake) bool {
	return !p.mixed && p.from == w.from
}

// mediaTurn is the chat as a media turn holds it.
type mediaTurn struct {
	turn   *chatTurn
	ctx    context.Context
	ticket uint64         // the sequence its message joins with
	from   sender         // whose media, in which thread and connection
	folded *pendingIntake // its sender's waiting text it took over, or nil
}

// beginMediaTurn takes the chat for a media message that is ready (its photos
// uploaded, its voice note transcribed), as a committed turn: media is never
// dropped, coalesced or restarted. It first reserves the media's ticket, the
// sequence its message joins with. Then it loops:
//
//  1. A waiting batch that holds text from before the ticket, which the media
//     may not take over, runs first, as a text turn of its own, registered or
//     collected behind the running turn (runAheadLocked).
//  2. With the chat free and no such batch waiting, the media turn registers,
//     taking over a waiting batch of its own sender's (admitMediaLocked).
//  3. Otherwise it waits: the ending turn hands it the chat (closeTurn), or
//     it tries again.
//
// Text that joins after the ticket is never the media turn's to answer: its
// message joins memory before that text (joinMedia), and it reads memory only
// up to its own message (snapshotForMedia). So a media turn answers only its
// own message, and its sender's text it took over.
//
// Text that joined memory before the ticket is never covered by the media
// turn either: from its join until its batch or turn is on record, it is on
// its way (joinText, landText), and the media turn waits for it; a batch
// leaves the buffer and its turn is registered in one step (flushIntake).
//
// The wait for the running turn does not watch parent: that turn derives
// from the same long-lived context, so if parent is cancelled it finishes
// promptly anyway, and registering over it would put two turns on one chat.
// The wait for text on its way does: when parent ends there, the media turn
// gives up, owning nothing, and its waiter is dropped.
func (b *Bot) beginMediaTurn(parent context.Context, to replyTarget, userID int64, row uint) (_ mediaTurn, ok bool) {
	k := to.conv()
	w := &mediaWaiter{
		ticket:  b.reserveSeq(),
		from:    sender{userID: userID, threadID: k.threadID, conn: to.businessConnectionID},
		to:      to,
		row:     row,
		parent:  parent,
		granted: make(chan mediaTurn, 1),
	}
	defer func() {
		if !ok { // it ends without owning a turn: a panic, or its context ended
			b.abandonMediaWaiter(k, w)
		}
	}()
	for {
		want, memoryGen := b.flushWant(k)
		step := b.mediaStep(k, w, b.getOrCreateChatMemory(k), want, memoryGen)
		if step.admitted {
			return step.media, true
		}
		if step.aheadTurn != nil {
			b.goTextTurn(step.aheadTurn, step.aheadCtx, step.ahead)
		}
		if step.onWay != nil {
			select {
			case <-step.onWay: // text from before the ticket is on its way: once on record, it goes first
			case <-parent.Done():
				return mediaTurn{}, false
			}
			continue
		}
		if step.running == nil {
			continue // a batch from before the ticket reached its window just now: it goes first
		}
		select {
		case m := <-w.granted:
			return m, true
		case <-step.running.done:
		}
	}
}

// abandonMediaWaiter takes w off its chat's queue when its goroutine ends
// without owning its turn; a turn already granted to it is ended, which hands
// the chat on.
func (b *Bot) abandonMediaWaiter(k convKey, w *mediaWaiter) {
	var granted *chatTurn
	func() {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		b.dropMediaWaiterLocked(k, w)
		select {
		case m := <-w.granted:
			granted = m.turn
		default:
		}
	}()
	if granted != nil {
		b.endTurn(granted)
	}
}

// mediaStep is one round of beginMediaTurn's loop, under turnsMu: the media
// turn was handed the chat, or registers on the free chat (admitted); or the
// text from before its ticket was started ahead of it (aheadTurn, for the
// caller to run) and it waits behind running.
type mediaStep struct {
	media     mediaTurn
	admitted  bool
	ahead     turnRequest
	aheadTurn *chatTurn
	aheadCtx  context.Context
	running   *chatTurn
	onWay     <-chan struct{} // the chat is free, but text from before the ticket is on its way
}

// mediaStep runs one round of beginMediaTurn's loop for w under turnsMu.
func (b *Bot) mediaStep(k convKey, w *mediaWaiter, chatMemory *ChatMemory, want, memoryGen uint64) (step mediaStep) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	select {
	case m := <-w.granted: // handed the chat while it looked around
		return mediaStep{media: m, admitted: true}
	default:
	}
	step.ahead, step.aheadTurn, step.aheadCtx = b.runAheadLocked(k, w, chatMemory, want, memoryGen)
	step.running = b.turns[k]
	if step.running == nil {
		if step.onWay = b.textOnWayBefore(k, w.ticket); step.onWay != nil {
			return step
		}
		step.media, step.admitted = b.admitMediaLocked(k, w)
		return step
	}
	b.waitForChatLocked(k, w)
	return step
}

// runAheadLocked takes the chat's waiting batch when it holds text from
// before w's ticket that w may not take over, and registers that text's turn
// or collects it behind the running turn (beginTextTurnLocked), in the same
// step in which w decides whether it may take the chat: that text is answered
// first, by a turn of its own, with its own sender, thread and context. want
// and memoryGen are the chat's flush want, read before the lock. A turn it
// registered is returned for the caller to run once it released the lock.
//
// Accepted trade-off: that batch's quiet window is cut short, so a sender
// still typing gets an answer to what they sent so far, and the rest in a
// turn of its own.
func (b *Bot) runAheadLocked(k convKey, w *mediaWaiter, chatMemory *ChatMemory, want, memoryGen uint64) (turnRequest, *chatTurn, context.Context) {
	ahead := func(p *pendingIntake) bool {
		if w.folds(p) || p.firstSeq > w.ticket {
			return false
		}
		// Logged before its turn is registered: nothing between that and the
		// caller taking the turn over can panic.
		InfoLogger.Printf("[%s] media in chat %v: the text waiting from before it runs first", b.config.ID, k)
		return true
	}
	started := b.startBatchLocked(k, ahead, chatMemory, want, memoryGen)
	return started.req, started.turn, started.ctx
}

// textOnWayBefore is textOnWayBeforeLocked taking intakeBuffersMu.
func (b *Bot) textOnWayBefore(k convKey, ticket uint64) <-chan struct{} {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	return b.textOnWayBeforeLocked(k, ticket)
}

// admitMediaLocked registers waiting media turn w on its free chat, unless a
// batch from before its ticket is waiting that it may not take over: that
// text goes first (runAheadLocked). A batch of its own sender's is taken
// over, to be read with the media; joinMedia decides whether it folds, and
// otherwise puts it back to run as its own text turn. The caller holds
// turnsMu, and takes intakeBuffersMu inside it.
func (b *Bot) admitMediaLocked(k convKey, w *mediaWaiter) (mediaTurn, bool) {
	folded, blocked, _ := b.batchForMedia(k, w)
	if blocked {
		return mediaTurn{}, false
	}
	if folded != nil { // logged before the turn is registered, as above
		InfoLogger.Printf("[%s] media turn in chat %v takes over %d waiting message(s) of its sender's",
			b.config.ID, k, folded.count)
	}
	b.dropMediaWaiterLocked(k, w)
	turn, ctx := b.registerTurnLocked(w.parent, k, 0, true)
	// What a text turn answering others' messages in its place needs, should
	// a clear of the media's sender stop it (stopForUserClearLocked): its
	// conversation, and where its reply goes.
	turn.req = turnRequest{ctx: w.parent, chatID: w.to.chatID, threadID: w.to.threadID, forum: w.to.forum,
		businessConnectionID: w.to.businessConnectionID}
	turn.mediaSeq = w.ticket // its message joins with its ticket
	turn.mediaFrom, turn.mediaRow = w.from.userID, w.row
	return mediaTurn{turn: turn, ctx: ctx, ticket: w.ticket, from: w.from, folded: folded}, true
}

// batchForMedia looks at the chat's waiting batch for media turn w, under
// intakeBuffersMu. A text message from before w's ticket still on its way to
// a batch or a turn blocks w (onWay is closed when one lands): once on
// record, it goes first, or folds. Then a batch with text from before w's
// ticket that w may not take over blocks it; a batch of w's own sender's is
// taken (folded).
func (b *Bot) batchForMedia(k convKey, w *mediaWaiter) (folded *pendingIntake, blocked bool, onWay <-chan struct{}) {
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	if onWay = b.textOnWayBeforeLocked(k, w.ticket); onWay != nil {
		return nil, true, onWay
	}
	pending := b.intakeBuffers[k]
	switch {
	case pending == nil:
		return nil, false, nil
	case w.folds(pending):
		return b.removeIntakeLocked(k), false, nil
	default:
		return nil, pending.firstSeq <= w.ticket, nil
	}
}

// grantMediaLocked is closeTurn handing the free chat to waiting media turn
// w: it admits w (admitMediaLocked) and wakes it. It returns the media turn,
// or nil when w may not have the chat yet.
func (b *Bot) grantMediaLocked(k convKey, w *mediaWaiter) *chatTurn {
	m, ok := b.admitMediaLocked(k, w)
	if !ok {
		return nil
	}
	w.granted <- m
	return m.turn
}

// collectLocked records work to run after turn as a text turn: a flush, or a
// batch the turn took over. The one wanting the newest message is kept, since
// its turn reads all of memory; the earliest message of any is remembered.
// The caller holds turnsMu.
func collectLocked(turn *chatTurn, req *turnRequest, want, first uint64) {
	if turn.rerun == nil || want > turn.rerunWant {
		turn.rerun, turn.rerunWant = req, want
	}
	turn.rerunFirst = earliest(turn.rerunFirst, first)
}

// waitForChatLocked queues w for its chat, once. The caller holds turnsMu.
func (b *Bot) waitForChatLocked(k convKey, w *mediaWaiter) {
	if b.mediaWaiters == nil {
		b.mediaWaiters = make(map[convKey][]*mediaWaiter)
	}
	if !slices.Contains(b.mediaWaiters[k], w) {
		b.mediaWaiters[k] = append(b.mediaWaiters[k], w)
	}
}

// dropMediaWaiterLocked takes w off its conversation's queue. The caller
// holds turnsMu.
func (b *Bot) dropMediaWaiterLocked(k convKey, w *mediaWaiter) {
	waiters := slices.DeleteFunc(b.mediaWaiters[k], func(x *mediaWaiter) bool { return x == w })
	if len(waiters) == 0 {
		delete(b.mediaWaiters, k)
		return
	}
	b.mediaWaiters[k] = waiters
}

// oldestMediaWaiterLocked is the conversation's waiting media turn with the
// oldest ticket, or nil. The caller holds turnsMu.
func (b *Bot) oldestMediaWaiterLocked(k convKey) *mediaWaiter {
	var oldest *mediaWaiter
	for _, w := range b.mediaWaiters[k] {
		if oldest == nil || w.ticket < oldest.ticket {
			oldest = w
		}
	}
	return oldest
}

// reserveSeq takes the next memory sequence for a message that joins later: a
// media turn's ticket.
func (b *Bot) reserveSeq() uint64 {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	b.memorySeq++
	return b.memorySeq
}

// joinMedia puts a media message into memory where its ticket holds its
// place (mediaSlot), with the ticket as its sequence: after everything that
// joined before the media was ready, everything already answered and the
// batch it took over; before text that joined later, which a turn of its own
// answers. It reports, like join, why the message did not join.
//
// The batch it took over folds only when no unanswered message from anyone
// else, or from another thread, lies between the ticket and the batch's
// newest message (G3): such a message, collected behind a turn, waiting or
// on its way, would otherwise sit inside the media turn's context. Then the
// media joins at its ticket alone, and the batch goes back to its quiet
// window (rebuffer), to run as its own text turn in F6 order. A batch whose
// media did not join goes back too. When a clear refused the media, the batch
// goes back only if a message of it is still in memory: text its sender sent
// after the clear (blind r5 #5); a batch the clear covered whole is dropped
// (K5).
func (b *Bot) joinMedia(message Message, media *mediaTurn) (Message, joinOutcome) {
	folded := media.folded
	declined := false
	held, outcome := b.join(message, func(chatMemory *ChatMemory) (int, uint64) {
		chatMemory.pinnedSeq.Store(media.ticket) // kept in the window until its turn reads it
		after := media.ticket
		if folded != nil {
			if otherTextBetween(chatMemory, media.ticket, folded.lastSeq, media.from) {
				declined = true
			} else {
				after = max(after, folded.lastSeq)
			}
		}
		return mediaSlot(chatMemory, after), media.ticket
	})
	switch {
	case folded == nil || (outcome == joinOK && !declined):
	case outcome == joinCleared && !b.batchInMemory(folded):
		// The clear that refused the media covered the batch it took over
		// (its sender's, from before), and dropped the chat's waiting batch:
		// that text goes too.
		InfoLogger.Printf("[%s] media turn %d in chat %v drops the cleared text it took over", b.config.ID, media.turn.seq, message.conv())
		media.folded = nil
	default:
		InfoLogger.Printf("[%s] media turn %d in chat %v does not fold its sender's waiting text", b.config.ID, media.turn.seq, message.conv())
		b.rebuffer(folded)
		media.folded = nil
	}
	return held, outcome
}

// batchInMemory reports whether a message of batch p is still in its
// conversation's memory: one no clear took out.
func (b *Bot) batchInMemory(p *pendingIntake) bool {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	mem := b.chatMemories[p.conv()]
	return mem != nil && slices.ContainsFunc(mem.Messages, func(m Message) bool {
		return m.IsUser && m.UserID == p.from.userID && m.Seq >= p.firstSeq && m.Seq <= p.lastSeq
	})
}

// otherTextBetween reports whether memory holds an unanswered user message
// with a sequence in (after, upTo] from someone other than from, or from
// another thread. The caller holds chatMemoriesMu.
func otherTextBetween(chatMemory *ChatMemory, after, upTo uint64, from sender) bool {
	answered := chatMemory.answeredThrough.Load()
	for _, m := range chatMemory.Messages {
		if m.IsUser && m.Seq > after && m.Seq <= upTo && m.Seq > answered &&
			(m.UserID != from.userID || m.ThreadID != from.threadID) {
			return true
		}
	}
	return false
}

// mediaSlot is where a media message goes in memory, strictly by its ticket:
// before the trailing user messages with a sequence above after, which
// joined after it was ready and are other people's text (answered or not: a
// reply stored aside leaves an answered question with no reply after it),
// after everything else; the scan stops at an assistant reply. The caller
// holds chatMemoriesMu.
func mediaSlot(chatMemory *ChatMemory, after uint64) int {
	at := len(chatMemory.Messages)
	for at > 0 {
		if m := chatMemory.Messages[at-1]; !m.IsUser || m.Seq <= after {
			break
		}
		at--
	}
	return at
}

// snapshotForMedia is snapshotForTurn for a media turn whose message joined
// with mediaSeq: its context ends with that message, so it never reads text
// that joined after the media was ready, and its reply goes right after it.
// unanswered is false when the message is no longer in memory (its sender's
// /clear took it out).
func (b *Bot) snapshotForMedia(turn *chatTurn, chatMemory *ChatMemory, mediaSeq uint64) (contextMessages []anthropic.BetaMessageParam, unanswered bool) {
	upTo, found := b.readUpTo(turn, chatMemory, mediaSeq)
	if !found {
		return nil, false
	}
	return b.buildContextMessages(upTo), true
}

// readUpTo copies chatMemory's messages up to and including the one with
// seq, and records on turn what they cover, in one step under the memory
// lock, as readForTurn does, or reports that the message is not there. The
// turn knows its media message from its admission (admitMediaLocked).
func (b *Bot) readUpTo(turn *chatTurn, chatMemory *ChatMemory, seq uint64) ([]Message, bool) {
	snapshotStarting(turn)
	defer snapshotTaken(turn) // once the lock below is released
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	chatMemory.pinnedSeq.CompareAndSwap(seq, 0) // read now: the window may trim it again
	end := slices.IndexFunc(chatMemory.Messages, func(m Message) bool { return m.Seq == seq })
	if end < 0 {
		return nil, false
	}
	upTo := slices.Clone(chatMemory.Messages[:end+1])
	b.markSnapshot(turn, newestAnswerableSeq(upTo), seq, chatMemory.generation) // chatMemoriesMu, then turnsMu
	return upTo, true
}

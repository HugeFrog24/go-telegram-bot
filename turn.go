package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"
)

// cancelReason records why a running turn's context was cancelled, so the
// turn can tell a deliberate interruption from a real failure.
type cancelReason int

const (
	cancelNone cancelReason = iota
	// cancelRestart: a newer message or an edit arrived before anything was
	// shown to the user. The turn is abandoned and rerun with it included.
	cancelRestart
	// cancelStop: the user pressed Telegram's Stop button under the draft.
	cancelStop
	// cancelClear: /clear or /clear_hard emptied the chat's history. The turn
	// ends silently: no reply, no rerun, nothing filed.
	cancelClear
)

// errTurnSilent marks a failure runTurn must not report: the turn body has
// either already told the user, or deliberately has nothing to say.
var errTurnSilent = errors.New("turn: failure handled by the turn body")

// turnRequest is everything one assistant turn needs to run and reply.
type turnRequest struct {
	ctx                                         context.Context
	chatID, userID                              int64
	username, firstName, lastName, languageCode string
	isPremium                                   bool
	messageTime                                 int
	businessConnectionID                        string
	threadID                                    int // forum topic of the message that produced the request; 0 outside topics
	isEmojiOnly                                 bool
}

// target is where this request's replies go.
func (r turnRequest) target() replyTarget {
	return replyTarget{chatID: r.chatID, businessConnectionID: r.businessConnectionID, threadID: r.threadID}
}

// chatTurn is one running assistant turn. At most one exists per chat.
//
// This is the second half of the debounce design. The intake buffer decides
// when a burst is quiet enough to answer; this decides what happens to a
// message that lands after that, while the model is already working. Without
// it every late fragment started another concurrent turn, and a slow typist
// got one reply per fragment, each split into several messages.
type chatTurn struct {
	seq    uint64
	chatID int64
	cancel context.CancelFunc
	done   chan struct{}

	// Guarded by Bot.turnsMu.
	draftID     string       // Telegram draft id shown for this turn, "" if none
	coveredSeq  uint64       // newest answerable user message (Message.Seq) this turn's context includes
	frontierSeq uint64       // newest message of any kind this turn's context includes
	memoryGen   uint64       // generation of the memory its context was read from; 0 before it read any
	committed   bool         // must not be restarted: output shown, or work not cheaply redone
	reason      cancelReason // why cancel was called, if it was
	rerun       *turnRequest // a flush that arrived mid-turn and may still need a turn
	rerunWant   uint64       // the newest answerable user message that flush wanted answered
}

func (t *chatTurn) draftIDString() string { return strconv.FormatUint(t.seq, 10) }

func (b *Bot) registerTurnLocked(parent context.Context, chatID int64, coveredSeq uint64, committed bool) (*chatTurn, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	b.turnSeq++
	t := &chatTurn{
		seq:        b.turnSeq,
		chatID:     chatID,
		cancel:     cancel,
		done:       make(chan struct{}),
		coveredSeq: coveredSeq,
		committed:  committed,
	}
	b.turns[chatID] = t
	return t, ctx
}

// beginTextTurn applies the late-message policy for a text flush. wantSeq is
// the newest answerable user message the flush wants answered. ok=false means
// the caller must not run a turn, because one of these happened instead:
//
//   - answered: a turn that already recorded its reply covered wantSeq, so
//     there is nothing left to answer. This is checked before anything is
//     registered or shown, under the lock finishTurn takes after the ending
//     turn raised answeredThrough;
//   - dropped: the running turn's context already includes wantSeq, so its
//     reply answers this flush too;
//   - restart: nothing has been shown yet, so the running turn is cancelled and
//     recorded to rerun with the new message in context. This is the only case
//     where tokens are wasted, and it is bounded by the time to first output;
//   - collect: output is already on screen, so the flush is recorded to run as
//     its own turn once this one finishes, unless the finished turn turns out to
//     have covered it after all (see finishTurn).
func (b *Bot) beginTextTurn(req turnRequest, wantSeq uint64) (*chatTurn, context.Context, bool) {
	chatMemory := b.getOrCreateChatMemory(req.chatID)
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()

	if wantSeq <= chatMemory.answeredThrough.Load() {
		InfoLogger.Printf("[%s] turn: chat %d flush dropped, message %d already answered",
			b.config.ID, req.chatID, wantSeq)
		return nil, nil, false
	}
	if running, exists := b.turns[req.chatID]; exists {
		switch {
		case wantSeq <= running.coveredSeq:
			InfoLogger.Printf("[%s] turn: chat %d flush dropped, message %d already covered by running turn %d",
				b.config.ID, req.chatID, wantSeq, running.seq)
		case !running.committed:
			running.reason = cancelRestart
			running.rerun, running.rerunWant = &req, wantSeq
			running.cancel()
			InfoLogger.Printf("[%s] turn: chat %d restarting turn %d, message %d arrived before any output",
				b.config.ID, req.chatID, running.seq, wantSeq)
		default:
			running.rerun, running.rerunWant = &req, wantSeq
			InfoLogger.Printf("[%s] turn: chat %d collected message %d, runs after turn %d",
				b.config.ID, req.chatID, wantSeq, running.seq)
		}
		return nil, nil, false
	}

	turn, ctx := b.registerTurnLocked(req.ctx, req.chatID, wantSeq, false)
	return turn, ctx, true
}

// beginTurn registers a turn for a media handler, waiting for any running turn
// on the chat to finish first. Media is never dropped or coalesced, and the
// turn is committed from the start: cancelling half-finished uploads or a voice
// transcription to save a few tokens is not worth the mess.
//
// The wait does not watch parent: the running turn derives from the same
// long-lived context, so if parent is cancelled it finishes promptly anyway,
// and registering over it would put two turns on one chat.
func (b *Bot) beginTurn(parent context.Context, chatID int64) (*chatTurn, context.Context) {
	for {
		b.turnsMu.Lock()
		running, exists := b.turns[chatID]
		if !exists {
			turn, ctx := b.registerTurnLocked(parent, chatID, 0, true)
			b.turnsMu.Unlock()
			return turn, ctx
		}
		b.turnsMu.Unlock()
		<-running.done
	}
}

// markSnapshot records the newest answerable user message and the newest
// message of any kind the turn's context includes, and the generation of the
// memory it read, under one lock. The turn's reply is filed right after the
// frontier (fileReply), in that memory only.
func (b *Bot) markSnapshot(turn *chatTurn, coveredSeq, frontierSeq, memoryGen uint64) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	turn.coveredSeq = max(turn.coveredSeq, coveredSeq)
	turn.frontierSeq = max(turn.frontierSeq, frontierSeq)
	if memoryGen != 0 {
		turn.memoryGen = memoryGen
	}
}

// contextEnd is where the turn's context ended: the newest message of any kind
// and the newest answerable user message it included, and the generation of
// the memory it read. The frontier and the generation are zero for a turn
// that never read memory, and all three are for a nil turn.
func (b *Bot) contextEnd(turn *chatTurn) (frontierSeq, coveredSeq, memoryGen uint64) {
	if turn == nil {
		return 0, 0, 0
	}
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return turn.frontierSeq, turn.coveredSeq, turn.memoryGen
}

// markCovered records the newest answerable user message the turn's context
// includes.
func (b *Bot) markCovered(turn *chatTurn, coveredSeq uint64) {
	b.markSnapshot(turn, coveredSeq, 0, 0)
}

// markCommitted flags that the user has seen output, so the turn can no longer
// be restarted, only collected behind.
func (b *Bot) markCommitted(turn *chatTurn) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	turn.committed = true
}

func (b *Bot) setTurnDraft(turn *chatTurn, draftID string) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	turn.draftID = draftID
}

func (b *Bot) turnReason(turn *chatTurn) cancelReason {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return turn.reason
}

// stopTurn handles Telegram's stopped_message_generation update. The draft id
// must match the running turn's, so a stale tap on an old draft cannot kill a
// newer turn. Reports whether a turn was cancelled.
func (b *Bot) stopTurn(chatID int64, draftID int) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()

	running, exists := b.turns[chatID]
	if !exists || running.draftID == "" || running.draftID != strconv.Itoa(draftID) {
		return false
	}
	running.reason = cancelStop
	running.cancel()
	return true
}

// restartForEdit reruns a turn that is reading the pre-edit text of the
// message at msgSeq and has shown nothing yet. Reports whether it did. A
// committed turn keeps going: the user has already seen a reply to the old
// wording.
func (b *Bot) restartForEdit(req turnRequest, msgSeq uint64) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()

	running, exists := b.turns[req.chatID]
	if !exists || running.committed || running.coveredSeq < msgSeq {
		return false
	}
	running.reason = cancelRestart
	running.rerun, running.rerunWant = &req, msgSeq
	running.cancel()
	return true
}

// cancelForClear stops the chat's running turn for /clear or /clear_hard: it
// ends silently, with no reply, and the flush collected behind it is
// dropped, since rerunning it against the emptied chat would answer history
// the user just deleted.
func (b *Bot) cancelForClear(chatID int64) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if running, exists := b.turns[chatID]; exists {
		running.reason = cancelClear
		running.rerun, running.rerunWant = nil, 0
		running.cancel()
	}
}

// clearedTurn reports whether /clear cancelled the turn (cancelForClear).
func (b *Bot) clearedTurn(turn *chatTurn) bool {
	return b.turnReason(turn) == cancelClear
}

// finishTurn unregisters the turn and returns the flush that still needs a turn
// of its own, if any.
//
// A restart always reruns: the cancelled turn delivered nothing. A collected
// flush reruns only if this turn's final context did not include its message.
// Media turns snapshot after their uploads or transcription, so a text that
// arrived during that work was collected and then answered anyway; rerunning
// it would produce a second, unprompted reply. Stop does not discard the
// flush: the message predates the tap and still deserves an answer.
func (b *Bot) finishTurn(turn *chatTurn) *turnRequest {
	b.turnsMu.Lock()
	if b.turns[turn.chatID] == turn {
		delete(b.turns, turn.chatID)
	}
	rerun, want, reason, covered := turn.rerun, turn.rerunWant, turn.reason, turn.coveredSeq
	b.turnsMu.Unlock()

	turn.cancel()
	close(turn.done)

	if rerun == nil {
		return nil
	}
	if reason != cancelRestart && want <= covered {
		InfoLogger.Printf("[%s] turn %d for chat %d covered collected message %d; no rerun",
			b.config.ID, turn.seq, turn.chatID, want)
		return nil
	}
	return rerun
}

// endTurn finishes the turn and runs whatever was collected behind it on its
// own goroutine, so a chain of collected turns does not nest on the stack of
// whichever update handler happened to run the first one.
func (b *Bot) endTurn(turn *chatTurn) {
	if next := b.finishTurn(turn); next != nil {
		go b.respondToChat(*next)
	}
}

// runTurn drives a registered turn to completion: keeps the typing indicator
// alive, picks the reply sink (draft or segments), runs call, delivers and
// records the reply, and afterwards runs anything collected while it was busy.
//
// call receives the turn's cancellable context and the sink, and returns the
// joined reply text. Both the debounced text path and the photo path go
// through here, so the two behave identically once the model is involved.
//
// Delivery of a finished reply uses the request's context, not the turn's: a
// Stop or restart that lands after the stream ended must not turn a complete
// reply into one that is stored but never sent.
//
// With progress notes on, the status message goes after the answer, but
// before an error reply or a stopped partial; on a restart the deferred close
// removes it, and the rerun starts its own if it writes notes.
func (b *Bot) runTurn(turn *chatTurn, turnCtx context.Context, req turnRequest,
	call func(ctx context.Context, out replyOutput) (string, error)) {
	defer b.endTurn(turn)

	stopTyping := b.startChatAction(turnCtx, req.target(), models.ChatActionTyping)
	defer stopTyping()

	out := b.newReplyOutput(turnCtx, turn, req)
	defer out.close()

	joined, err := call(turnCtx, out)
	if b.clearedTurn(turn) {
		InfoLogger.Printf("[%s] turn %d for chat %d dropped: the chat's history was cleared", b.config.ID, turn.seq, req.chatID)
		return
	}
	if err != nil {
		if errors.Is(err, errTurnSilent) {
			return
		}
		switch b.turnReason(turn) {
		case cancelRestart:
			InfoLogger.Printf("[%s] turn %d for chat %d abandoned for restart", b.config.ID, turn.seq, req.chatID)
			return
		case cancelStop:
			b.deliverStoppedTurn(turn, req, out)
			return
		}
		ErrorLogger.Printf("Error getting Anthropic response: %v", err)
		out.dropStatus()
		if sendErr := b.sendTurnReply(req.ctx, turn, req.target(), b.anthropicErrorResponse(err, req.userID)); sendErr != nil {
			ErrorLogger.Printf("Error sending response: %v", sendErr)
		}
		return
	}

	if finishErr := out.finish(req.ctx, joined); finishErr != nil {
		ErrorLogger.Printf("[%s] delivering reply to chat %d: %v", b.config.ID, req.chatID, finishErr)
		return
	}
	if _, storeErr := b.recordTurnReply(turn, req.chatID, joined); storeErr != nil {
		ErrorLogger.Printf("Error recording assistant turn: %v", storeErr)
	}
}

// deliverStoppedTurn keeps what the user watched being written. Telegram drops
// the draft shortly after a stop unless the bot re-sends it as a real message,
// and memory must hold the same text the user saw, or the next turn continues
// from a reply that was never delivered.
func (b *Bot) deliverStoppedTurn(turn *chatTurn, req turnRequest, out replyOutput) {
	out.dropStatus()
	partial := strings.TrimSpace(out.partial())
	InfoLogger.Printf("[%s] turn %d for chat %d stopped by user with %d chars shown",
		b.config.ID, turn.seq, req.chatID, len(partial))
	if partial == "" {
		return
	}
	if err := b.sendLongMessage(req.ctx, req.target(), partial); err != nil {
		ErrorLogger.Printf("[%s] sending stopped partial to chat %d: %v", b.config.ID, req.chatID, err)
		return
	}
	if _, err := b.recordTurnReply(turn, req.chatID, partial); err != nil {
		ErrorLogger.Printf("Error recording stopped assistant turn: %v", err)
	}
}

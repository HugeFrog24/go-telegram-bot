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
	threadID                                    int  // forum topic of the message that produced the request; 0 outside topics
	forum                                       bool // threadID is a forum supergroup's topic, a conversation of its own (replyTarget.forum)
	isEmojiOnly                                 bool
	memoryGen                                   uint64
	// editRestart: an edit restarted the turn this request reruns. No later
	// edit restarts it again (restartForEdit), so an edit loop costs one
	// extra model request at most.
	editRestart bool
	// joinedSeq is the memory sequence of the message that produced the
	// request, and firstSeq that of the first message of its batch: the
	// earliest message the turn is for. Zero when unknown.
	joinedSeq, firstSeq uint64
}

// target is where this request's replies go.
func (r turnRequest) target() replyTarget {
	return replyTarget{chatID: r.chatID, businessConnectionID: r.businessConnectionID, threadID: r.threadID, forum: r.forum}
}

// sender is who sent a message, in which conversation's topic and through
// which Business connection.
type sender struct {
	userID   int64
	threadID int // the conversation's topic (convKey), as Message.ThreadID
	conn     string
}

func (r turnRequest) sender() sender {
	return sender{userID: r.userID, threadID: r.conv().threadID, conn: r.businessConnectionID}
}

// chatTurn is one running assistant turn. At most one exists per
// conversation (convKey).
//
// This is the second half of the debounce design. The intake buffer decides
// when a burst is quiet enough to answer; this decides what happens to a
// message that lands after that, while the model is already working. Without
// it every late fragment started another concurrent turn, and a slow typist
// got one reply per fragment, each split into several messages.
type chatTurn struct {
	seq    uint64
	conv   convKey
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
	// firstSeq is the earliest message a text turn was registered for, and
	// rerunFirst the earliest message of the work collected behind it: what
	// closeTurn compares with a waiting media turn's ticket.
	firstSeq, rerunFirst uint64
	// req is a text turn's own request, which a restart without a new flush
	// reruns (stopForUserClearLocked).
	req turnRequest
	// replyAside: a clear of one sender's messages landed after this
	// committed turn read them, and it also answers others: its reply is
	// sent, but stored aside, never in memory or a later context.
	replyAside bool
	// shown: the turn's answer is on its way to the user (markCommitted): its
	// draft or first segment is out, or its reply is being filed and sent
	// (recordTurnReply). A media turn is committed from its registration, as
	// it is never restarted, but has shown nothing until then.
	shown bool
	// mediaSeq is a media turn's own message: its ticket, set at admission
	// (admitMediaLocked), which its message joins with.
	mediaSeq uint64
	// mediaFrom and mediaRow are a media turn's message's sender and row id,
	// recorded at admission: a clear of the sender's messages stops the turn
	// by them, even once the window has trimmed the message out of memory
	// (stopMediaForUserClearLocked).
	mediaFrom int64
	mediaRow  uint
}

// earliest is the lowest of the non-zero sequences, or 0 when all are zero.
func earliest(seqs ...uint64) uint64 {
	var first uint64
	for _, s := range seqs {
		if s != 0 && (first == 0 || s < first) {
			first = s
		}
	}
	return first
}

func (t *chatTurn) draftIDString() string { return strconv.FormatUint(t.seq, 10) }

func (b *Bot) registerTurnLocked(parent context.Context, k convKey, coveredSeq uint64, committed bool) (*chatTurn, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	b.turnSeq++
	t := &chatTurn{
		seq:        b.turnSeq,
		conv:       k,
		cancel:     cancel,
		done:       make(chan struct{}),
		coveredSeq: coveredSeq,
		committed:  committed,
	}
	b.turns[k] = t
	return t, ctx
}

// beginTextTurnLocked applies the late-message policy for a text flush. wantSeq
// is the newest answerable user message the flush wants answered. The caller
// holds turnsMu, and read the chat's memory before taking it. ok=false means
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
func (b *Bot) beginTextTurnLocked(req turnRequest, wantSeq uint64, chatMemory *ChatMemory) (*chatTurn, context.Context, bool) {
	first := earliest(req.firstSeq, wantSeq)
	if req.memoryGen != 0 && req.memoryGen != chatMemory.generation {
		InfoLogger.Printf("[%s] turn: chat %v flush dropped, its history was cleared", b.config.ID, req.conv())
		return nil, nil, false
	}
	if wantSeq <= chatMemory.answeredThrough.Load() {
		InfoLogger.Printf("[%s] turn: chat %v flush dropped, message %d already answered",
			b.config.ID, req.conv(), wantSeq)
		return nil, nil, false
	}
	if running, exists := b.turns[req.conv()]; exists {
		switch {
		case running.reason == cancelClear:
			// The cleared turn is ending silently: the flush runs as a
			// fresh turn after it, and leaves the reason alone.
			running.rerun, running.rerunWant = &req, wantSeq
			running.rerunFirst = earliest(running.rerunFirst, first)
			InfoLogger.Printf("[%s] turn: chat %d collected message %d behind cleared turn %d",
				b.config.ID, req.chatID, wantSeq, running.seq)
		case wantSeq <= running.coveredSeq:
			InfoLogger.Printf("[%s] turn: chat %d flush dropped, message %d already covered by running turn %d",
				b.config.ID, req.chatID, wantSeq, running.seq)
		case !running.committed:
			running.reason = cancelRestart
			running.rerun, running.rerunWant = &req, wantSeq
			running.rerunFirst = earliest(running.rerunFirst, running.firstSeq, first)
			running.cancel()
			InfoLogger.Printf("[%s] turn: chat %d restarting turn %d, message %d arrived before any output",
				b.config.ID, req.chatID, running.seq, wantSeq)
		default:
			running.rerun, running.rerunWant = &req, wantSeq
			running.rerunFirst = earliest(running.rerunFirst, first)
			InfoLogger.Printf("[%s] turn: chat %d collected message %d, runs after turn %d",
				b.config.ID, req.chatID, wantSeq, running.seq)
		}
		return nil, nil, false
	}

	turn, ctx := b.registerTurnLocked(req.ctx, req.conv(), wantSeq, false)
	turn.firstSeq, turn.req = first, req
	return turn, ctx, true
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

// markCommitted flags that the turn's answer is on its way to the user: its
// draft or first segment is out, or its reply is about to be filed and sent.
// The turn can no longer be restarted, only collected behind, and a clear
// from here on sets its reply aside rather than stopping it.
func (b *Bot) markCommitted(turn *chatTurn) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	turn.committed, turn.shown = true, true
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
func (b *Bot) stopTurn(k convKey, draftID int) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()

	running, exists := b.turns[k]
	if !exists || running.draftID == "" || running.draftID != strconv.Itoa(draftID) || running.reason == cancelClear {
		return false
	}
	running.reason = cancelStop
	running.cancel()
	return true
}

// restartForEdit reruns a turn that is reading the pre-edit text of the
// message at msgSeq and has shown nothing yet. Reports whether it did. A
// committed turn keeps going: the user has already seen a reply to the old
// wording. Edits restart a given turn's work at most once (B6): a turn that
// is itself an edit's rerun is not restarted again; the edit is collected
// behind it as any flush is, and as the turn read the message, does not
// rerun it (finishTurn).
func (b *Bot) restartForEdit(req turnRequest, msgSeq uint64) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()

	running := b.editTargetLocked(req.conv(), msgSeq)
	if running == nil {
		return false
	}
	if running.req.editRestart {
		collectLocked(running, &req, msgSeq, msgSeq)
		return false
	}
	req.editRestart = true
	running.reason = cancelRestart
	running.rerun, running.rerunWant = &req, msgSeq
	running.rerunFirst = earliest(running.rerunFirst, running.firstSeq, msgSeq)
	running.cancel()
	return true
}

// editTargetLocked is the running turn of conversation k that an edit of the
// message at msgSeq would restart or be collected behind: one that has shown
// nothing, read the message, and was not stopped by a clear; or nil. The
// caller holds turnsMu.
func (b *Bot) editTargetLocked(k convKey, msgSeq uint64) *chatTurn {
	running, exists := b.turns[k]
	if !exists || running.committed || running.coveredSeq < msgSeq || running.reason == cancelClear {
		return nil
	}
	return running
}

// editWouldAct reports whether an edit of the message at msgSeq in
// conversation k would re-arm a quiet window or restart a turn, both of
// which a sender over their rate limit may not do (B6).
func (b *Bot) editWouldAct(k convKey, msgSeq uint64) bool {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if b.editTargetLocked(k, msgSeq) != nil {
		return true
	}
	b.intakeBuffersMu.Lock() // turnsMu, then intakeBuffersMu
	defer b.intakeBuffersMu.Unlock()
	_, waiting := b.intakeBuffers[k]
	return waiting
}

// clearedTurn reports whether /clear cancelled the turn (beginClear). The
// reason is sticky: no Stop, edit or flush can overwrite it.
func (b *Bot) clearedTurn(turn *chatTurn) bool {
	return b.turnReason(turn) == cancelClear
}

// claimDelivery is runTurn's step between the model call and delivering its
// reply, under turnsMu: a turn a clear stopped or a restart cancelled is
// reported so; any other is committed, so no restart lands from here on
// (draft mode's finish closes the draft and sends after this).
func (b *Bot) claimDelivery(turn *chatTurn) cancelReason {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if turn.reason != cancelClear && turn.reason != cancelRestart {
		turn.committed = true
	}
	return turn.reason
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
	rerun, _, _ := b.closeTurn(turn, false)
	return rerun
}

// endTurn finishes the turn and hands the chat to the flush collected behind
// it, which runs on its own goroutine, so a chain of collected turns does not
// nest on the stack of whichever update handler happened to run the first one.
func (b *Bot) endTurn(turn *chatTurn) {
	if rerun, next, nextCtx := b.closeTurn(turn, true); next != nil {
		b.goTextTurn(next, nextCtx, *rerun)
	}
}

// closeTurn is finishTurn. In the same step that unregisters this turn, it
// hands the chat on, so no other turn can take it in between:
//
//   - with handOver, to a rerun that still has something to answer and was
//     collected before the oldest waiting media turn took its ticket: text
//     sent before the media goes first;
//   - otherwise to that media turn (F6: steady text cannot starve it), with
//     the rerun, if any, collected behind it; unless a batch from before its
//     ticket is waiting that it may not take over, which beginMediaTurn then
//     runs first;
//   - otherwise, with handOver, to the rerun.
//
// A text turn handed the chat is returned for the caller to run; a media turn
// runs on its own goroutine.
func (b *Bot) closeTurn(turn *chatTurn, handOver bool) (*turnRequest, *chatTurn, context.Context) {
	defer func() {
		turn.cancel()
		close(turn.done)
	}()
	h := b.handOn(turn, handOver, b.getOrCreateChatMemory(turn.conv))
	switch {
	case h.rerun == nil:
		return nil, nil, nil
	case !h.due:
		InfoLogger.Printf("[%s] turn %d for chat %v covered collected message %d; no rerun",
			b.config.ID, turn.seq, turn.conv, h.want)
		return nil, nil, nil
	case h.runnable && h.media != nil:
		InfoLogger.Printf("[%s] turn: chat %v collected message %d runs after waiting media turn %d",
			b.config.ID, turn.conv, h.want, h.media.seq)
		return nil, nil, nil
	case handOver && h.next == nil:
		InfoLogger.Printf("[%s] turn: chat %v collected message %d already answered; no rerun",
			b.config.ID, turn.conv, h.want)
	}
	return h.rerun, h.next, h.nextCtx
}

// handover is what closeTurn decided in its one step under turnsMu.
type handover struct {
	rerun         *turnRequest
	want          uint64
	due, runnable bool
	next          *chatTurn // the text turn handed the chat
	nextCtx       context.Context
	media         *chatTurn // the media turn handed the chat
}

// handOn unregisters turn and hands the chat on, in one step under turnsMu
// (closeTurn).
func (b *Bot) handOn(turn *chatTurn, handOver bool, chatMemory *ChatMemory) (h handover) {
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if b.turns[turn.conv] == turn {
		delete(b.turns, turn.conv)
	}
	rerun, want, reason, covered := turn.rerun, turn.rerunWant, turn.reason, turn.coveredSeq
	first := earliest(turn.rerunFirst, want)
	h.rerun, h.want = rerun, want
	h.due = rerun != nil && (reason == cancelRestart || reason == cancelClear || want > covered) // a cleared turn filed nothing
	h.runnable = h.due && handOver && want > chatMemory.answeredThrough.Load() &&
		(rerun.memoryGen == 0 || rerun.memoryGen == chatMemory.generation)
	if b.turns[turn.conv] != nil {
		return h
	}
	waiter := b.oldestMediaWaiterLocked(turn.conv)
	textFirst := h.runnable && (waiter == nil || first <= waiter.ticket)
	if !textFirst && waiter != nil {
		h.media = b.grantMediaLocked(turn.conv, waiter)
	}
	switch {
	case h.media != nil && h.runnable:
		collectLocked(h.media, rerun, want, first)
	case h.media == nil && h.runnable:
		h.next, h.nextCtx = b.registerTurnLocked(rerun.ctx, turn.conv, want, false)
		h.next.firstSeq, h.next.req = first, *rerun
	}
	return h
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
	switch b.claimDelivery(turn) {
	case cancelClear:
		InfoLogger.Printf("[%s] turn %d for chat %d dropped: the chat's history was cleared", b.config.ID, turn.seq, req.chatID)
		return
	case cancelRestart:
		// Also when the call finished before it noticed: the turn showed
		// nothing, and its rerun answers in its place.
		InfoLogger.Printf("[%s] turn %d for chat %d abandoned for restart", b.config.ID, turn.seq, req.chatID)
		return
	}
	if err != nil {
		if errors.Is(err, errTurnSilent) {
			return
		}
		if b.turnReason(turn) == cancelStop {
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
	if _, storeErr := b.recordTurnReply(turn, req.conv(), joined); storeErr != nil && !errors.Is(storeErr, errStaleReply) {
		ErrorLogger.Printf("Error recording assistant turn: %v", storeErr)
	}
}

// deliverStoppedTurn keeps what the user watched being written. Telegram drops
// the draft shortly after a stop unless the bot re-sends it as a real message,
// and memory must hold the same text the user saw, or the next turn continues
// from a reply that was never delivered.
func (b *Bot) deliverStoppedTurn(turn *chatTurn, req turnRequest, out replyOutput) {
	out.dropStatus()
	if b.replyIsStale(turn) {
		return // the history the partial belongs to was cleared
	}
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
	if _, err := b.recordTurnReply(turn, req.conv(), partial); err != nil {
		ErrorLogger.Printf("Error recording stopped assistant turn: %v", err)
	}
}

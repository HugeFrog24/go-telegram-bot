package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
)

const (
	// defaultStatusFirstDelay is how long a note waits before the first status
	// message goes out, so a note the answer overtakes at once never flickers
	// on screen.
	defaultStatusFirstDelay = 1 * time.Second
	// defaultStatusEditInterval throttles edits of the status message to
	// Telegram's general guidance of about one message per second per chat.
	defaultStatusEditInterval = 1 * time.Second
	// businessRightsTTL is how long a getBusinessConnection answer is reused.
	businessRightsTTL = 10 * time.Minute
	// statusShutdownGrace bounds how long open status messages may hold up a
	// shutdown: the final delete of each, and the wait for all of them.
	statusShutdownGrace = 5 * time.Second
)

// statusLine shows a turn's progress notes as one silent Telegram message,
// edited in place as newer notes arrive and deleted once the answer is out.
//
// Like draftStream, a single sender goroutine makes every Telegram call, so
// the calls are strictly sequential and never run on the model's stream
// goroutine; note, answerSent and freeze only record what should happen and
// wake it. The sender stops when the turn's context is done or close is
// called, but every call runs on the delivery context: cancelling a send in
// flight could leave a message on screen whose ID was never learned, and so
// could never be deleted. close makes the final delete itself, once the
// sender has exited, so the calls stay sequential.
type statusLine struct {
	b            *Bot
	to           replyTarget
	firstDelay   time.Duration
	editInterval time.Duration
	deliverCtx   context.Context
	// shutdownGrace bounds the final delete once deliverCtx is gone.
	shutdownGrace time.Duration

	ctx       context.Context // the turn's context; close cancels it too
	cancel    context.CancelFunc
	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	started   bool
	closed    bool
	ended     bool      // no more sends or edits: frozen, failed or not allowed
	pending   string    // newest note not shown yet; "" when none
	pendingAt time.Time // arrival of the earliest note still waiting
	dropShown bool      // answerSent wants the shown message gone

	// Owned by the sender goroutine; close reads them only after it exits.
	msgID         int // the status message on screen; 0 when none
	shown         string
	lastCall      time.Time // end of the previous send or edit
	rightsChecked bool
}

// newStatusLine builds the status line; start launches the sender. turnCtx
// bounds the sender, deliverCtx carries its Telegram calls and must outlive a
// restart, so close can still delete what was shown.
func (b *Bot) newStatusLine(turnCtx, deliverCtx context.Context, to replyTarget) *statusLine {
	ctx, cancel := context.WithCancel(turnCtx)
	return &statusLine{
		b:             b,
		to:            to,
		firstDelay:    b.statusFirstDelay,
		editInterval:  b.statusEditInterval,
		deliverCtx:    deliverCtx,
		shutdownGrace: statusShutdownGrace,
		ctx:           ctx,
		cancel:        cancel,
		kick:          make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
}

// start launches the sender. From here until close has finished, the line
// counts as open for the bot's shutdown wait.
func (s *statusLine) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started && !s.closed {
		s.started = true
		s.b.statusLines.add()
		go s.run()
	}
}

// note records the newest note and wakes the sender. It never blocks on
// Telegram; a note that is overtaken before it is shown is simply dropped.
func (s *statusLine) note(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	text = clampRunes(text, telegramMessageLimit)
	s.mu.Lock()
	if s.closed || s.ended {
		s.mu.Unlock()
		return
	}
	if s.pending == "" {
		s.pendingAt = time.Now()
	}
	s.pending = text
	s.mu.Unlock()
	s.wake()
}

// answerSent is for segment mode: answer text just reached the user, so the
// waiting note is dropped and the shown message deleted, keeping the status
// line from sitting above text the user has read. The next note starts a new
// status message below the answer. After close it has no effect.
func (s *statusLine) answerSent() {
	s.mu.Lock()
	s.pending = ""
	s.dropShown = true
	s.mu.Unlock()
	s.wake()
}

// dropWaiting is for draft mode: answer text is streaming into the draft, so
// a note still waiting to be shown is dropped. Unlike freeze it does not end
// the notes, and unlike answerSent it leaves a shown message alone.
func (s *statusLine) dropWaiting() {
	s.mu.Lock()
	s.pending = ""
	s.mu.Unlock()
}

// freeze ends notes for the turn: the waiting note is dropped and later
// notes are ignored, but a shown message stays until answerSent or close
// removes it. Draft mode calls it once the first block of answer text is
// final; the sender calls it after a failed call or a Business refusal.
func (s *statusLine) freeze() {
	s.mu.Lock()
	s.pending = ""
	s.ended = true
	s.mu.Unlock()
}

// close stops the sender, waits for any call in flight, then deletes the
// shown message. It is idempotent, and once it returns no call is in flight
// and none will be made.
//
// Once the delivery context is gone, the process is shutting down: the
// message on screen must still go, so the delete gets a fresh context that
// gives it shutdownGrace at most.
func (s *statusLine) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.pending = ""
		started := s.started
		s.mu.Unlock()
		s.cancel()
		if !started {
			return
		}
		<-s.done

		ctx := s.deliverCtx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), s.shutdownGrace)
			defer cancel()
		}
		s.deleteShown(ctx)
		s.b.statusLines.done()
	})
}

func (s *statusLine) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// run is the sender loop: wake on a note, an answerSent or a due timer, make
// every call that is due, and sleep until the next one is.
func (s *statusLine) run() {
	defer close(s.done)

	var due <-chan time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-due:
		}
		due = nil
		for {
			act, wait, stop := s.next(time.Now())
			if stop {
				return
			}
			if act == nil {
				if wait > 0 {
					due = time.After(wait)
				}
				break
			}
			act()
		}
	}
}

// next decides, under the lock, what the sender does now. It returns stop
// once the sender must exit, or the call to make, or when there is none, how
// long until one is due (0: nothing waits).
//
// A pending delete goes first. A note waiting while nothing is shown is sent
// once the earliest waiting note is firstDelay old, after the Business rights
// check; with a message shown, the newest note replaces it no sooner than
// editInterval after the previous call, unless its text is already shown.
func (s *statusLine) next(now time.Time) (act func(), wait time.Duration, stop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.ctx.Err() != nil {
		return nil, 0, true
	}
	if s.dropShown {
		s.dropShown = false
		if s.msgID != 0 {
			return func() { s.deleteShown(s.deliverCtx) }, 0, false
		}
	}
	if s.ended || s.pending == "" {
		return nil, 0, false
	}

	if s.msgID == 0 {
		if d := s.pendingAt.Add(s.firstDelay).Sub(now); d > 0 {
			return nil, d, false
		}
		if s.to.businessConnectionID != "" && !s.rightsChecked {
			return s.checkRights, 0, false
		}
		text := s.pending
		s.pending = ""
		return func() { s.send(text) }, 0, false
	}

	if s.pending == s.shown {
		s.pending = ""
		return nil, 0, false
	}
	if d := s.lastCall.Add(s.editInterval).Sub(now); d > 0 {
		return nil, d, false
	}
	text := s.pending
	s.pending = ""
	return func() { s.edit(text) }, 0, false
}

// checkRights runs once per turn, before the first send in a Business chat.
func (s *statusLine) checkRights() {
	s.rightsChecked = true
	if s.b.businessNotesAllowed(s.deliverCtx, s.to.businessConnectionID) {
		return
	}
	s.freeze()
	InfoLogger.Printf("[%s] no progress notes in business chat %d this turn: connection %s does not allow them",
		s.b.config.ID, s.to.chatID, s.to.businessConnectionID)
}

func (s *statusLine) send(text string) {
	msg, err := s.b.tgBot.SendMessage(s.deliverCtx, &bot.SendMessageParams{
		ChatID:               s.to.chatID,
		MessageThreadID:      s.to.threadID,
		BusinessConnectionID: s.to.businessConnectionID,
		Text:                 text,
		DisableNotification:  true,
	})
	if err == nil && (msg == nil || msg.ID == 0) {
		err = errors.New("sendMessage returned no message id")
	}
	if err != nil {
		s.failed("send", err)
		return
	}
	s.msgID, s.shown, s.lastCall = msg.ID, text, time.Now()
}

func (s *statusLine) edit(text string) {
	_, err := s.b.tgBot.EditMessageText(s.deliverCtx, &bot.EditMessageTextParams{
		ChatID:               s.to.chatID,
		MessageID:            s.msgID,
		BusinessConnectionID: s.to.businessConnectionID,
		Text:                 text,
	})
	// Telegram rejects an edit to identical text; the text is on screen either way.
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		s.failed("edit", err)
		return
	}
	s.shown, s.lastCall = text, time.Now()
}

// failed ends notes for the turn. Notes are cosmetic, so the turn carries on,
// and a message already shown is still deleted later.
func (s *statusLine) failed(call string, err error) {
	s.freeze()
	if s.deliverCtx.Err() == nil {
		InfoLogger.Printf("[%s] status message %s in chat %d failed, no more notes this turn: %v",
			s.b.config.ID, call, s.to.chatID, err)
	}
}

// deleteShown deletes the status message on screen, if any. Business
// messages need deleteBusinessMessages; deleteMessage has no connection ID.
func (s *statusLine) deleteShown(ctx context.Context) {
	id := s.msgID
	if id == 0 {
		return
	}
	s.msgID, s.shown = 0, ""

	var err error
	if s.to.businessConnectionID != "" {
		_, err = s.b.tgBot.DeleteBusinessMessages(ctx, &bot.DeleteBusinessMessagesParams{
			BusinessConnectionID: s.to.businessConnectionID,
			MessageIDs:           []int{id},
		})
	} else {
		_, err = s.b.tgBot.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: s.to.chatID, MessageID: id})
	}
	if err != nil {
		// The one case where a status message is left behind.
		ErrorLogger.Printf("[%s] could not delete status message %d in chat %d: %v",
			s.b.config.ID, id, s.to.chatID, err)
		if s.to.businessConnectionID != "" {
			// The rights may have changed mid-turn: look them up again next turn.
			s.b.forgetBusinessRights(s.to.businessConnectionID)
		}
	}
}

// openStatusLines counts a bot's status lines from start until close has
// finished, so a shutdown can wait for their messages to be removed.
type openStatusLines struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed once n drops to zero; nil while nobody waits
}

func (o *openStatusLines) add() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
}

func (o *openStatusLines) done() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n--
	if o.n == 0 && o.idle != nil {
		close(o.idle)
		o.idle = nil
	}
}

// whenIdle returns a channel that is closed once no line is open.
func (o *openStatusLines) whenIdle() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.idle == nil {
		o.idle = make(chan struct{})
	}
	idle := o.idle
	if o.n == 0 {
		close(idle)
		o.idle = nil
	}
	return idle
}

// waitForStatusLines waits until every status line the bot started has been
// closed, or until timeout, and reports whether they all were. A shutdown
// calls it, so a turn cut short still removes its status message.
func (b *Bot) waitForStatusLines(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-b.statusLines.whenIdle():
		return true
	case <-timer.C:
		ErrorLogger.Printf("[%s] shutdown: status messages still open after %v; some may be left in chats",
			b.config.ID, timeout)
		return false
	}
}

// businessRightsEntry is one cached getBusinessConnection answer.
type businessRightsEntry struct {
	allowed bool
	fetched time.Time
}

// businessNotesAllowed reports whether a Business connection lets the bot
// show progress notes: it must be enabled and able to send messages (which
// covers editing them) and delete them afterwards. Every successful lookup,
// allowing or not, is cached for businessRightsTTL; a failed lookup means no
// notes and is retried next time.
func (b *Bot) businessNotesAllowed(ctx context.Context, connectionID string) bool {
	b.businessRightsMu.Lock()
	entry, ok := b.businessRights[connectionID]
	b.businessRightsMu.Unlock()
	if ok && b.clock.Now().Sub(entry.fetched) < businessRightsTTL {
		return entry.allowed
	}

	conn, err := b.tgBot.GetBusinessConnection(ctx, &bot.GetBusinessConnectionParams{BusinessConnectionID: connectionID})
	if err == nil && conn == nil {
		err = errors.New("getBusinessConnection returned no connection")
	}
	if err != nil {
		ErrorLogger.Printf("[%s] business rights lookup for connection %s failed, no progress notes: %v",
			b.config.ID, connectionID, err)
		return false
	}
	allowed := conn.IsEnabled && conn.Rights != nil && conn.Rights.CanReply && conn.Rights.CanDeleteSentMessages

	b.businessRightsMu.Lock()
	b.businessRights[connectionID] = businessRightsEntry{allowed: allowed, fetched: b.clock.Now()}
	b.businessRightsMu.Unlock()
	return allowed
}

// forgetBusinessRights drops the cached rights of a connection, so the next
// lookup asks Telegram again.
func (b *Bot) forgetBusinessRights(connectionID string) {
	b.businessRightsMu.Lock()
	defer b.businessRightsMu.Unlock()
	delete(b.businessRights, connectionID)
}

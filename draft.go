package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
)

const (
	// draftUpdateInterval throttles sendMessageDraft. Telegram documents no
	// limit for drafts specifically, so this matches its general guidance of
	// about one send per second per chat before 429s start.
	draftUpdateInterval = 1 * time.Second
	// draftKeepaliveInterval re-sends the current draft during silence, such
	// as a server-side web search. The preview lives roughly 30 seconds when
	// untouched; whether updates extend it is undocumented, so this keeps
	// well inside that either way.
	draftKeepaliveInterval = 10 * time.Second
	// telegramMessageLimit is the text cap, in characters, for both drafts
	// and sent messages.
	telegramMessageLimit = 4096
)

// draftEligible reports whether a turn may stream into a Telegram draft.
// Telegram documents drafts for private chats only, and its reference lists
// no business_connection_id parameter for sendMessageDraft (the Go library's
// struct carries the field, but nothing says the server honours it), so
// business traffic keeps the segment path until that is verified live.
// A forum topic does not change eligibility; the draft just carries its thread id.
func (b *Bot) draftEligible(to replyTarget) bool {
	return b.config.StreamDrafts && to.businessConnectionID == "" && to.chatID > 0
}

// draftStream is one growing Telegram draft for one turn.
//
// A single sender goroutine owns every sendMessageDraft call, so sends are
// strictly sequential, never run on the model's stream goroutine, and cannot
// land at Telegram out of order. The stream goroutine only records the newest
// text and signals; the sender throttles, keeps the draft alive through silent
// stretches, and goes quiet after the first failure so a broken draft degrades
// to a plain final message.
type draftStream struct {
	b         *Bot
	to        replyTarget
	draftID   string
	interval  time.Duration
	keepalive time.Duration
	after     func(time.Duration) <-chan time.Time // how the sender sleeps; time.After outside tests
	onFirst   func()

	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	latest string // newest text from the model; "" when none of it is answer
	shown  string // last answer text Telegram accepted; "" while latest is ""
	failed bool
}

// newDraftStream builds the stream; start launches the sender. The two are
// separate so tests can shorten the intervals before anything runs.
func (b *Bot) newDraftStream(parent context.Context, to replyTarget, draftID string, onFirst func()) *draftStream {
	ctx, cancel := context.WithCancel(parent)
	return &draftStream{
		b:         b,
		to:        to,
		draftID:   draftID,
		interval:  b.draftInterval,
		keepalive: b.draftKeepalive,
		after:     b.draftAfter,
		onFirst:   onFirst,
		ctx:       ctx,
		cancel:    cancel,
		kick:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

func (d *draftStream) start() { go d.run() }

// update records the full reply text so far and wakes the sender. It never
// blocks on Telegram.
//
// An empty text means nothing of the answer is on screen any more: what the
// draft showed turned out to be a progress note. From then on current is ""
// and the draft is not kept alive, until answer text arrives again.
func (d *draftStream) update(text string) {
	d.mu.Lock()
	d.latest = text
	if text == "" {
		d.shown = ""
	}
	d.mu.Unlock()
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// run is the sender loop: wake on new text or on the keepalive deadline,
// respect the throttle, send the newest text, repeat until closed or failed.
func (d *draftStream) run() {
	defer close(d.done)

	var lastSend time.Time
	for {
		var keepalive <-chan time.Time
		if !lastSend.IsZero() {
			keepalive = d.after(d.keepalive - time.Since(lastSend))
		}
		woken := false
		select {
		case <-d.ctx.Done():
			return
		case <-d.kick:
		case <-keepalive:
			woken = true
		}

		if wait := d.interval - time.Since(lastSend); !lastSend.IsZero() && wait > 0 {
			select {
			case <-d.ctx.Done():
				return
			case <-d.after(wait):
			}
		}

		d.mu.Lock()
		text, shown := d.latest, d.shown
		d.mu.Unlock()
		if text == "" {
			// Nothing to show, and an empty draft is never sent. Disarm the
			// keepalive until answer text arrives; that send is a first one.
			lastSend = time.Time{}
			continue
		}
		if text == shown && !woken {
			continue
		}
		if !d.send(text) {
			return
		}
		lastSend = time.Now()
	}
}

// send performs one sendMessageDraft call and reports whether the draft is
// still usable afterwards.
func (d *draftStream) send(text string) bool {
	text = clampRunes(text, telegramMessageLimit)
	_, err := d.b.tgBot.SendMessageDraft(d.ctx, &bot.SendMessageDraftParams{
		ChatID:          d.to.chatID,
		MessageThreadID: d.to.threadID,
		DraftID:         d.draftID,
		Text:            text,
		CanStop:         true,
		KeepOnStop:      true,
	})
	if err != nil {
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
		if d.ctx.Err() == nil {
			// Cosmetic from here on: the final message is still sent.
			InfoLogger.Printf("[%s] draft %s for chat %d failed, falling back to final message only: %v",
				d.b.config.ID, d.draftID, d.to.chatID, err)
		}
		return false
	}

	d.mu.Lock()
	first := d.shown == ""
	if d.latest != "" { // else the text became a progress note while in flight
		d.shown = text
	}
	d.mu.Unlock()
	if first && d.onFirst != nil {
		d.onFirst()
	}
	return true
}

// current returns the answer text the user has actually seen.
func (d *draftStream) current() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.shown
}

// close stops the sender and waits for any send in flight, so nothing lands
// on the chat after the final message. Telegram removes the draft itself once
// the bot sends a message.
func (d *draftStream) close() {
	d.closeOnce.Do(func() {
		d.cancel()
		<-d.done
	})
}

// replyOutput is the sink a turn writes into. Two implementations: a draft
// that grows in place and becomes one final message, or the original
// one-message-per-text-block segments. With progress notes on, either one
// also drives the turn's status line; see withStatus.
type replyOutput struct {
	onSegment  func(string) error
	onProgress func(string)
	onNote     func(string) // nil when notes are off
	finish     func(ctx context.Context, joined string) error
	partial    func() string
	dropStatus func() // delete the status message now and end notes; idempotent; no-op when off
	close      func()
}

// sinks is what the turn's model call writes into.
func (o replyOutput) sinks() replySinks {
	return replySinks{onSegment: o.onSegment, onProgress: o.onProgress, onNote: o.onNote}
}

// newReplyOutput builds the turn's sink. With notes on, the status line's
// Telegram calls run on the request's context, like the final message's, so
// a restart or a Stop cannot cancel the delete of a message already shown.
func (b *Bot) newReplyOutput(turnCtx context.Context, turn *chatTurn, req turnRequest) replyOutput {
	to := req.target()
	drafting := b.draftEligible(to)
	out := b.newAnswerOutput(turnCtx, turn, to, drafting)
	if status := b.startStatusLine(turnCtx, req.ctx, to); status != nil {
		out = out.withStatus(status, drafting)
	}
	return out
}

// newAnswerOutput is the sink for the answer alone, exactly as turns
// delivered it before progress notes existed.
func (b *Bot) newAnswerOutput(turnCtx context.Context, turn *chatTurn, to replyTarget, drafting bool) replyOutput {
	if drafting {
		draftID := turn.draftIDString()
		d := b.newDraftStream(turnCtx, to, draftID, func() { b.markCommitted(turn) })
		b.setTurnDraft(turn, draftID)
		d.start()
		return replyOutput{
			onProgress: d.update,
			finish: func(ctx context.Context, joined string) error {
				d.close()
				return b.sendLongMessage(ctx, to, joined)
			},
			partial:    d.current,
			dropStatus: noStatus,
			close:      d.close,
		}
	}

	return replyOutput{
		onSegment: func(seg string) error {
			b.markCommitted(turn)
			return b.sendOneSegment(turnCtx, to, seg)
		},
		finish:     func(context.Context, string) error { return nil },
		partial:    func() string { return "" },
		dropStatus: noStatus,
		close:      func() {},
	}
}

// noStatus stands in for the status line's calls on a turn without progress
// notes.
func noStatus() {
	// Notes are off: there is no status message to drop, freeze or close.
}

// withStatus makes the output show the turn's progress notes on status and
// remove the status message once the answer is out. The answer always goes
// first, so a delete that fails never costs the user the answer.
//
//   - Segments: once a segment has reached the user, the status message is
//     deleted at once, so it never sits above answer text already read; the
//     next note starts a new one below. After a failed send nothing reached
//     the user, and the status message stays until the turn closes.
//   - Draft: the answer becomes a real message only in finish. A note still
//     waiting when answer text streams into the draft is dropped, notes stop
//     as soon as answer text is final, because sending a status message would
//     clear the draft mid-answer, and the status message is deleted once the
//     final message is out.
//
// dropStatus and close end the status line too: runTurn drops it before an
// error reply or a stopped partial, and close removes it after a restart.
func (o replyOutput) withStatus(status *statusLine, drafting bool) replyOutput {
	closeAnswer := o.close
	o.onNote = status.note
	o.dropStatus = status.close
	o.close = func() {
		closeAnswer()
		status.close()
	}

	if drafting {
		progress, finish := o.onProgress, o.finish
		o.onProgress = func(text string) {
			if text != "" {
				status.dropWaiting()
			}
			progress(text)
		}
		o.onSegment = func(string) error {
			status.freeze()
			return nil
		}
		o.finish = func(ctx context.Context, joined string) error {
			err := finish(ctx, joined)
			status.close()
			return err
		}
		return o
	}

	send := o.onSegment
	o.onSegment = func(seg string) error {
		if err := send(seg); err != nil {
			return err
		}
		status.answerSent()
		return nil
	}
	return o
}

// startStatusLine starts the status line of a turn that shows progress
// notes, or returns nil when notes are off. turnCtx bounds its sender and
// deliverCtx carries its Telegram calls (see newStatusLine). Whoever starts
// one must close it on every path, once the turn's reply is out.
func (b *Bot) startStatusLine(turnCtx, deliverCtx context.Context, to replyTarget) *statusLine {
	if !b.config.NotesEnabled() {
		return nil
	}
	status := b.newStatusLine(turnCtx, deliverCtx, to)
	status.start()
	return status
}

// sendLongMessage delivers text as one message, or as few as the character
// cap allows, splitting on paragraph and line boundaries before cutting words.
func (b *Bot) sendLongMessage(ctx context.Context, to replyTarget, text string) error {
	for _, chunk := range splitTelegramMessage(text, telegramMessageLimit) {
		if err := b.sendOneSegment(ctx, to, chunk); err != nil {
			return err
		}
	}
	return nil
}

// splitTelegramMessage splits text into pieces of at most limit runes,
// preferring paragraph breaks, then line breaks, then spaces.
func splitTelegramMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []string
	for {
		off := runeOffset(text, limit)
		if off < 0 {
			return append(out, text)
		}
		window := text[:off]
		cut := -1
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i > 0 {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = off
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
		if text == "" {
			return out
		}
	}
}

// runeOffset returns the byte offset just past the n-th rune of s, or -1 when
// s holds at most n runes.
func runeOffset(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return -1
}

// clampRunes truncates s to at most n runes.
func clampRunes(s string, n int) string {
	if off := runeOffset(s, n); off >= 0 {
		return s[:off]
	}
	return s
}

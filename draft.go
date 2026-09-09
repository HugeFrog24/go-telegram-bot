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
func (b *Bot) draftEligible(chatID int64, businessConnectionID string) bool {
	return b.config.StreamDrafts && businessConnectionID == "" && chatID > 0
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
	chatID    int64
	draftID   string
	interval  time.Duration
	keepalive time.Duration
	onFirst   func()

	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	latest string // newest text from the model
	shown  string // last text Telegram accepted
	failed bool
}

// newDraftStream builds the stream; start launches the sender. The two are
// separate so tests can shorten the intervals before anything runs.
func (b *Bot) newDraftStream(parent context.Context, chatID int64, draftID string, onFirst func()) *draftStream {
	ctx, cancel := context.WithCancel(parent)
	return &draftStream{
		b:         b,
		chatID:    chatID,
		draftID:   draftID,
		interval:  draftUpdateInterval,
		keepalive: draftKeepaliveInterval,
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
func (d *draftStream) update(text string) {
	d.mu.Lock()
	d.latest = text
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
			keepalive = time.After(d.keepalive - time.Since(lastSend))
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
			case <-time.After(wait):
			}
		}

		d.mu.Lock()
		text, shown := d.latest, d.shown
		d.mu.Unlock()
		if text == "" || (text == shown && !woken) {
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
		ChatID:     d.chatID,
		DraftID:    d.draftID,
		Text:       text,
		CanStop:    true,
		KeepOnStop: true,
	})
	if err != nil {
		d.mu.Lock()
		d.failed = true
		d.mu.Unlock()
		if d.ctx.Err() == nil {
			// Cosmetic from here on: the final message is still sent.
			InfoLogger.Printf("[%s] draft %s for chat %d failed, falling back to final message only: %v",
				d.b.config.ID, d.draftID, d.chatID, err)
		}
		return false
	}

	d.mu.Lock()
	first := d.shown == ""
	d.shown = text
	d.mu.Unlock()
	if first && d.onFirst != nil {
		d.onFirst()
	}
	return true
}

// current returns the text the user has actually seen.
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
// one-message-per-text-block segments.
type replyOutput struct {
	onSegment  func(string) error
	onProgress func(string)
	finish     func(ctx context.Context, joined string) error
	partial    func() string
	close      func()
}

func (b *Bot) newReplyOutput(turnCtx context.Context, turn *chatTurn, chatID int64, businessConnectionID string) replyOutput {
	if b.draftEligible(chatID, businessConnectionID) {
		draftID := turn.draftIDString()
		d := b.newDraftStream(turnCtx, chatID, draftID, func() { b.markCommitted(turn) })
		b.setTurnDraft(turn, draftID)
		d.start()
		return replyOutput{
			onProgress: d.update,
			finish: func(ctx context.Context, joined string) error {
				d.close()
				return b.sendLongMessage(ctx, chatID, joined, businessConnectionID)
			},
			partial: d.current,
			close:   d.close,
		}
	}

	return replyOutput{
		onSegment: func(seg string) error {
			b.markCommitted(turn)
			return b.sendOneSegment(turnCtx, chatID, seg, businessConnectionID)
		},
		finish:  func(context.Context, string) error { return nil },
		partial: func() string { return "" },
		close:   func() {},
	}
}

// sendLongMessage delivers text as one message, or as few as the character
// cap allows, splitting on paragraph and line boundaries before cutting words.
func (b *Bot) sendLongMessage(ctx context.Context, chatID int64, text, businessConnectionID string) error {
	for _, chunk := range splitTelegramMessage(text, telegramMessageLimit) {
		if err := b.sendOneSegment(ctx, chatID, chunk, businessConnectionID); err != nil {
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

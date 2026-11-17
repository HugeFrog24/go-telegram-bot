package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func largestPhotoSize(photos []models.PhotoSize) models.PhotoSize {
	if len(photos) == 0 {
		return models.PhotoSize{}
	}
	largest := photos[0]
	largestArea := largest.Width * largest.Height
	for i := 1; i < len(photos); i++ {
		area := photos[i].Width * photos[i].Height
		if area > largestArea {
			largest = photos[i]
			largestArea = area
		}
	}
	return largest
}

// downloadTelegramFile fetches a file the bot received. The download link
// carries the bot's token (…/file/bot<TOKEN>/…), and Go's *url.Error puts the
// whole link in its message, so no error it returns says more than the
// operation, the file id, a status and the cause, with the link and the token
// cut out (withoutLink): its callers log them.
func (b *Bot) downloadTelegramFile(ctx context.Context, fileID string) ([]byte, error) {
	fileInfo, err := b.tgBot.GetFile(ctx, &tgbot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("telegram GetFile %s: %w", fileID, err) // the library redacts its own links
	}
	downloadURL := b.tgBot.FileDownloadLink(fileInfo)
	scrub := func(op string, err error) error {
		return fmt.Errorf("telegram download %s: %s: %w", fileID, op, withoutLink(err, downloadURL, b.config.TelegramToken))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, scrub("building the request", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, scrub("request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download %s: status %d", fileID, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, scrub("reading the body", err)
	}
	return data, nil
}

// linkFreeError is an error whose message had a link and a token cut out; it
// unwraps to the cause, not to an error that still carries them.
type linkFreeError struct {
	msg   string
	cause error
}

func (e *linkFreeError) Error() string { return e.msg }
func (e *linkFreeError) Unwrap() error { return e.cause }

// withoutLink is err with link and secret cut out of its message: a
// *url.Error gives way to its cause (the link is its URL field), and any copy
// left in the cause's text is replaced too. errors.Is still sees the cause,
// so a cancelled download still reads as context.Canceled.
func withoutLink(err error, link, secret string) error {
	cause := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	msg := cause.Error()
	for _, s := range []string{link, secret} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return &linkFreeError{msg: msg, cause: cause}
}

// replyTarget is where a reply goes: the chat, the business connection the
// message arrived through, and the forum topic it was sent in. Every send path
// takes one, so no reply can forget a part of it.
type replyTarget struct {
	chatID               int64
	businessConnectionID string
	threadID             int // forum topic; 0 outside topics
	// shared: the chat's history is not the sender's alone, as in a group,
	// a supergroup or a Business chat. False for a private chat with the bot.
	shared bool
	// forum: threadID is a topic of a forum supergroup, and so names a
	// conversation of its own (conv). A topic in a private chat with the bot
	// is replied in, but is not one.
	forum bool
}

// targetFor builds the target for replying to msg. Telegram also sets
// message_thread_id on plain reply threads in non-forum supergroups, where
// sending it back is an error, so the thread is only used for topic messages
// (is_topic_message: a topic of a forum supergroup, or of a private chat
// with the bot).
func targetFor(msg *models.Message, businessConnectionID string) replyTarget {
	to := replyTarget{chatID: msg.Chat.ID, businessConnectionID: businessConnectionID}
	switch msg.Chat.Type {
	case models.ChatTypeGroup, models.ChatTypeSupergroup, models.ChatTypeChannel:
		to.shared = true
	}
	if businessConnectionID != "" {
		to.shared = true
	}
	if msg.IsTopicMessage {
		to.threadID = msg.MessageThreadID
		to.forum = msg.Chat.Type == models.ChatTypeSupergroup && businessConnectionID == ""
	}
	return to
}

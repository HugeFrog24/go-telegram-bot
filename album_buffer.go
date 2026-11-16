package main

import (
	"context"
	"sort"
	"time"

	"github.com/go-telegram/bot/models"
)

const albumFlushWindow = 1 * time.Second

type pendingAlbum struct {
	items                                       []*models.Message
	to                                          replyTarget
	userID                                      int64
	username, firstName, lastName, languageCode string
	isPremium                                   bool
	messageTime                                 int
	timer                                       *time.Timer
	pending                                     *turnRequest // text batch the album took over (takeIntake)
}

func (b *Bot) bufferAlbumItem(
	ctx context.Context,
	msg *models.Message,
	to replyTarget,
	userID int64,
	username, firstName, lastName string,
	isPremium bool,
	languageCode string,
	messageTime int,
	pending *turnRequest,
) {
	b.albumBuffersMu.Lock()
	defer b.albumBuffersMu.Unlock()

	album, exists := b.albumBuffers[msg.MediaGroupID]
	if !exists {
		album = &pendingAlbum{
			to:           to,
			userID:       userID,
			username:     username,
			firstName:    firstName,
			lastName:     lastName,
			isPremium:    isPremium,
			languageCode: languageCode,
			messageTime:  messageTime,
		}
		b.albumBuffers[msg.MediaGroupID] = album
	}
	album.items = append(album.items, msg)
	if pending != nil {
		album.pending = pending
	}

	if album.timer != nil {
		album.timer.Stop()
	}
	mediaGroupID := msg.MediaGroupID
	album.timer = time.AfterFunc(albumFlushWindow, func() {
		b.flushAlbum(ctx, mediaGroupID)
	})
}

func (b *Bot) flushAlbum(ctx context.Context, mediaGroupID string) {
	b.albumBuffersMu.Lock()
	album, exists := b.albumBuffers[mediaGroupID]
	if !exists {
		b.albumBuffersMu.Unlock()
		return
	}
	delete(b.albumBuffers, mediaGroupID)
	items := album.items
	captured := *album
	b.albumBuffersMu.Unlock()

	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	if !b.checkRateLimits(captured.userID) {
		b.sendRateLimitExceededMessage(ctx, captured.to)
		b.answerPending(captured.pending)
		return
	}

	b.handlePhotoMessage(
		ctx, items, captured.pending,
		captured.to, captured.userID,
		captured.username, captured.firstName, captured.lastName,
		captured.isPremium, captured.languageCode, captured.messageTime,
	)
}

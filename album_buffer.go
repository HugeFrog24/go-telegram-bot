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
	chatID, userID                              int64
	username, firstName, lastName, languageCode string
	isPremium                                   bool
	messageTime                                 int
	businessConnectionID                        string
	timer                                       *time.Timer
}

func (b *Bot) bufferAlbumItem(
	ctx context.Context,
	msg *models.Message,
	chatID, userID int64,
	username, firstName, lastName string,
	isPremium bool,
	languageCode string,
	messageTime int,
	businessConnectionID string,
) {
	b.albumBuffersMu.Lock()
	defer b.albumBuffersMu.Unlock()

	album, exists := b.albumBuffers[msg.MediaGroupID]
	if !exists {
		album = &pendingAlbum{
			chatID:               chatID,
			userID:               userID,
			username:             username,
			firstName:            firstName,
			lastName:             lastName,
			isPremium:            isPremium,
			languageCode:         languageCode,
			messageTime:          messageTime,
			businessConnectionID: businessConnectionID,
		}
		b.albumBuffers[msg.MediaGroupID] = album
	}
	album.items = append(album.items, msg)

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
		b.sendRateLimitExceededMessage(ctx, captured.chatID, captured.businessConnectionID)
		return
	}

	b.handlePhotoMessage(
		ctx, items,
		captured.chatID, captured.userID,
		captured.username, captured.firstName, captured.lastName,
		captured.isPremium, captured.languageCode, captured.messageTime,
		captured.businessConnectionID,
	)
}

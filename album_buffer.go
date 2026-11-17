package main

import (
	"context"
	"sort"
	"time"

	"github.com/go-telegram/bot/models"
)

const albumFlushWindow = 1 * time.Second

type pendingAlbum struct {
	items []*models.Message
	// row is the album's message, stored aside when its first item arrived
	// (storePhotoArrival); stored is false when that store failed.
	row                                         Message
	stored                                      bool
	to                                          replyTarget
	userID                                      int64
	username, firstName, lastName, languageCode string
	isPremium                                   bool
	messageTime                                 int
	timer                                       *time.Timer
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
) {
	b.albumBuffersMu.Lock()
	defer b.albumBuffersMu.Unlock()

	key := albumKey{conv: to.conv(), mediaGroupID: msg.MediaGroupID}
	album, exists := b.albumBuffers[key]
	if !exists {
		row, err := b.storePhotoArrival(msg, to)
		if err != nil {
			ErrorLogger.Printf("[%s] store album message failed: %v", b.config.ID, err)
		}
		album = &pendingAlbum{
			row:          row,
			stored:       err == nil,
			to:           to,
			userID:       userID,
			username:     username,
			firstName:    firstName,
			lastName:     lastName,
			isPremium:    isPremium,
			languageCode: languageCode,
			messageTime:  messageTime,
		}
		b.albumBuffers[key] = album
	}
	album.items = append(album.items, msg)

	if album.timer != nil {
		album.timer.Stop()
	}
	album.timer = time.AfterFunc(albumFlushWindow, func() {
		defer b.recoverPanic("album flush")
		b.flushAlbum(ctx, key)
	})
}

func (b *Bot) flushAlbum(ctx context.Context, key albumKey) {
	captured, exists := b.takeAlbum(key)
	if !exists {
		return
	}
	items := captured.items

	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	if !captured.stored {
		b.sendNotice(ctx, captured.to, "Sorry, I had trouble saving your message.")
		return
	}

	if !b.checkRateLimits(captured.userID) {
		b.sendRateLimitExceededMessage(ctx, captured.to)
		return
	}

	b.handlePhotoMessage(
		ctx, items, captured.row,
		captured.to, captured.userID,
		captured.username, captured.firstName, captured.lastName,
		captured.isPremium, captured.languageCode, captured.messageTime,
	)
}

// takeAlbum removes the album's buffer and returns it, under albumBuffersMu.
func (b *Bot) takeAlbum(key albumKey) (pendingAlbum, bool) {
	b.albumBuffersMu.Lock()
	defer b.albumBuffersMu.Unlock()
	album, exists := b.albumBuffers[key]
	if !exists {
		return pendingAlbum{}, false
	}
	delete(b.albumBuffers, key)
	return *album, true
}

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

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

func (b *Bot) downloadTelegramFile(ctx context.Context, fileID string) ([]byte, error) {
	fileInfo, err := b.tgBot.GetFile(ctx, &tgbot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("telegram GetFile %s: %w", fileID, err)
	}
	downloadURL := b.tgBot.FileDownloadLink(fileInfo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram download request %s: %w", fileID, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram download %s: %w", fileID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download %s: status %d", fileID, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram download read %s: %w", fileID, err)
	}
	return data, nil
}

// replyTarget is where a reply goes: the chat, the business connection the
// message arrived through, and the forum topic it was sent in. Every send path
// takes one, so no reply can forget a part of it.
type replyTarget struct {
	chatID               int64
	businessConnectionID string
	threadID             int // forum topic; 0 outside topics
}

// targetFor builds the target for replying to msg. Telegram also sets
// message_thread_id on plain reply threads in non-forum supergroups, where
// sending it back is an error, so the thread is only used for topic messages.
func targetFor(msg *models.Message, businessConnectionID string) replyTarget {
	to := replyTarget{chatID: msg.Chat.ID, businessConnectionID: businessConnectionID}
	if msg.IsTopicMessage {
		to.threadID = msg.MessageThreadID
	}
	return to
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const fileNotFoundPrefix = "File not found: "

func formatUploadFilename(botID uint, chatID int64, tgMessageID int, ext string) string {
	return fmt.Sprintf("tg-%d-%d-%d.%s", botID, chatID, tgMessageID, ext)
}

func (b *Bot) uploadImageToAnthropic(ctx context.Context, data []byte, filename, contentType string) (string, error) {
	resp, err := b.anthropicClient.Files.Upload(ctx, anthropic.FileUploadParams{
		File: anthropic.File(bytes.NewReader(data), filename, contentType),
	})
	if err != nil {
		return "", fmt.Errorf("anthropic files upload: %w", err)
	}
	return resp.ID, nil
}

func (b *Bot) deleteFileFromAnthropic(ctx context.Context, fileID string) error {
	_, err := b.anthropicClient.Files.Delete(ctx, fileID, anthropic.FileDeleteParams{})
	if err == nil {
		return nil
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("anthropic files delete %s: %w", fileID, err)
}

func (b *Bot) compensatingDelete(ctx context.Context, fileIDs []string) {
	for _, fid := range fileIDs {
		if err := b.deleteFileFromAnthropic(ctx, fid); err != nil {
			ErrorLogger.Printf("[%s] compensating delete for %s: %v", b.config.ID, fid, err)
		}
	}
}

func extractMissingFileID(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return ""
	}
	if apiErr.StatusCode != http.StatusNotFound {
		return ""
	}
	return parseMissingFileIDFromBody(apiErr.RawJSON())
}

func parseMissingFileIDFromBody(raw string) string {
	idx := strings.Index(raw, fileNotFoundPrefix)
	if idx == -1 {
		return ""
	}
	rest := raw[idx+len(fileNotFoundPrefix):]
	end := strings.IndexFunc(rest, func(r rune) bool {
		return (r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') &&
			r != '_'
	})
	if end == -1 {
		return rest
	}
	return rest[:end]
}

func (b *Bot) hardDeleteScope(ctx context.Context, query string, args ...interface{}) error {
	var rows []Message
	if err := b.db.Unscoped().Where(query, args...).Find(&rows).Error; err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := b.db.Where(query, args...).Delete(&Message{}).Error; err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}

	hardDeletable := make([]uint, 0, len(rows))
	for _, row := range rows {
		if b.deleteRowFiles(ctx, row) {
			hardDeletable = append(hardDeletable, row.ID)
		}
	}
	if len(hardDeletable) == 0 {
		return nil
	}
	if err := b.db.Unscoped().Where("id IN ?", hardDeletable).Delete(&Message{}).Error; err != nil {
		return fmt.Errorf("hard delete: %w", err)
	}
	return nil
}

func (b *Bot) deleteRowFiles(ctx context.Context, row Message) bool {
	if len(row.ImageFileIDs) == 0 {
		return true
	}
	allOk := true
	for _, fid := range row.ImageFileIDs {
		if err := b.deleteFileFromAnthropic(ctx, fid); err != nil {
			ErrorLogger.Printf("[%s] anthropic delete %s (row %d): %v", b.config.ID, fid, row.ID, err)
			allOk = false
		}
	}
	return allOk
}

func stripDeadFileIDs(src []string, deadSet map[string]struct{}) (survivors []string, dirty bool) {
	survivors = make([]string, 0, len(src))
	for _, fid := range src {
		if _, dead := deadSet[fid]; dead {
			dirty = true
			continue
		}
		survivors = append(survivors, fid)
	}
	return survivors, dirty
}

func (b *Bot) markFilesPendingCleanup(ctx context.Context, chatID int64, deadFileIDs []string) (int, error) {
	if len(deadFileIDs) == 0 {
		return 0, nil
	}
	deadSet := make(map[string]struct{}, len(deadFileIDs))
	for _, id := range deadFileIDs {
		deadSet[id] = struct{}{}
	}
	var rows []Message
	if err := b.db.WithContext(ctx).
		Where("bot_id = ? AND chat_id = ? AND image_file_ids IS NOT NULL", b.botID, chatID).
		Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("scan rows for cleanup: %w", err)
	}
	now := time.Now()
	updated := 0
	for _, row := range rows {
		survivors, dirty := stripDeadFileIDs(row.ImageFileIDs, deadSet)
		if !dirty {
			continue
		}
		if len(survivors) == 0 {
			row.ImageFileIDs = nil
			row.FilesCleanedAt = &now
		} else {
			row.ImageFileIDs = survivors
		}
		if err := b.db.WithContext(ctx).Save(&row).Error; err != nil {
			return updated, fmt.Errorf("update row %d: %w", row.ID, err)
		}
		updated++
	}
	return updated, nil
}

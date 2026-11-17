package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"gorm.io/gorm"
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

// compensatingDelete deletes Files API uploads that no message will hold.
//
// The deletes run detached from ctx, which a /clear may have cancelled with
// the photo's turn, under a bounded timeout of their own. A delete that
// fails is logged with its file id only; a durable retry is Task H.
func (b *Bot) compensatingDelete(ctx context.Context, fileIDs []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensatingDeleteTimeout)
	defer cancel()
	for _, fid := range fileIDs {
		if err := b.deleteFileFromAnthropic(ctx, fid); err != nil {
			ErrorLogger.Printf("[%s] compensating delete for %s failed: %v", b.config.ID, fid, err)
		}
	}
}

// compensatingDeleteTimeout bounds the deletes of one compensation.
const compensatingDeleteTimeout = 30 * time.Second

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

// hardDeleteScope permanently deletes the rows matching query up to cutoff,
// with their Files API files. The rows are soft-deleted first, then their
// files deleted, then the rows hard-deleted by the same scope and cutoff,
// except any whose files could not all be deleted: those stay soft-deleted,
// so their file ids are not lost.
func (b *Bot) hardDeleteScope(ctx context.Context, cutoff uint, query string, args ...interface{}) error {
	scope := func() *gorm.DB { return b.db.Where(query, args...).Where("id <= ?", cutoff) }
	var rows []Message
	if err := scope().Unscoped().Find(&rows).Error; err != nil {
		return fmt.Errorf("scan rows: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if err := scope().Delete(&Message{}).Error; err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}

	var kept []uint
	for _, row := range rows {
		if !b.deleteRowFiles(ctx, row) {
			kept = append(kept, row.ID)
		}
	}
	hard := scope().Unscoped()
	if len(kept) > 0 {
		hard = hard.Where("id NOT IN ?", kept)
	}
	if err := hard.Delete(&Message{}).Error; err != nil {
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

func (b *Bot) markFilesPendingCleanup(ctx context.Context, k convKey, deadFileIDs []string) (int, error) {
	if len(deadFileIDs) == 0 {
		return 0, nil
	}
	deadSet := make(map[string]struct{}, len(deadFileIDs))
	for _, id := range deadFileIDs {
		deadSet[id] = struct{}{}
	}
	var rows []Message
	scope, args := b.convScope(k)
	if err := b.db.WithContext(ctx).Where(scope, args...).
		Where("image_file_ids IS NOT NULL").
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
		columns := []string{"ImageFileIDs"}
		if len(survivors) == 0 {
			row.ImageFileIDs = nil
			row.FilesCleanedAt = &now
			columns = append(columns, "FilesCleanedAt")
		} else {
			row.ImageFileIDs = survivors
		}
		// Only these columns, and only while the row is live: a Save of the
		// copy read above would bring back a row a /clear deleted since (its
		// upsert), or insert it again after a /clear_hard.
		result := b.db.WithContext(ctx).Model(&Message{}).Where("id = ?", row.ID).Select(columns).
			Updates(&Message{ImageFileIDs: row.ImageFileIDs, FilesCleanedAt: row.FilesCleanedAt})
		if result.Error != nil {
			return updated, fmt.Errorf("update row %d: %w", row.ID, result.Error)
		}
		updated += int(result.RowsAffected)
	}
	return updated, nil
}

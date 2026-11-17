package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogs records both loggers for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	prevInfo, prevErr := InfoLogger, ErrorLogger
	InfoLogger, ErrorLogger = log.New(&logged, "", 0), log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger, ErrorLogger = prevInfo, prevErr })
	return &logged
}

// fileLinkAt makes every Telegram file download go to base, the way the
// library builds the link: …/file/bot<TOKEN>/<path>.
func fileLinkAt(tg *MockTelegramClient, base, token string) {
	tg.GetFileFunc = func(context.Context, *bot.GetFileParams) (*models.File, error) {
		return &models.File{FilePath: "voice/file_7.oga"}, nil
	}
	tg.FileDownloadLinkFunc = func(f *models.File) string { return base + "/file/bot" + token + "/" + f.FilePath }
}

// Blind prefill finding 8 (B7): a failed Telegram file download never
// returns or logs the bot's token, which the download link carries; the
// error still says what failed.
func TestDownloadTelegramFile_ErrorsNeverCarryTheToken(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const token = "123456:SECRET-TOKEN-abcdefghijklmnop"
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(notFound.Close)
	for name, tc := range map[string]struct {
		base string
		says string
	}{
		"failing transport": {base: "http://127.0.0.1:1", says: "connect"},
		"404 response":      {base: notFound.URL, says: "status 404"},
	} {
		t.Run(name, func(t *testing.T) {
			logged := captureLogs(t)
			b, tg := setupBotForTest(t, ownerChat)
			b.config.TelegramToken = token
			fileLinkAt(tg, tc.base, token)
			recordSends(tg)

			_, err := b.downloadTelegramFile(context.Background(), "file-7")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), token)
			assert.NotContains(t, err.Error(), "SECRET")
			assert.Contains(t, err.Error(), "file-7", "the error names the file")
			assert.Contains(t, err.Error(), tc.says, "the error keeps what failed")

			// The voice path logs the failure.
			b.config.ElevenLabsAPIKey = "test"
			b.sttURL, b.ttsURL = "http://127.0.0.1:1", "http://127.0.0.1:1/"
			b.handleUpdate(context.Background(), nil, &models.Update{Message: voiceMessage(ownerChat, 70)})
			// And the photo path.
			b.handleUpdate(context.Background(), nil, photoUpdate(71))
			require.Contains(t, logged.String(), "voice-file", "the voice failure was logged")
			require.Contains(t, logged.String(), "photo-file", "the photo failure was logged")
			assert.NotContains(t, logged.String(), "SECRET", "no log line carries the token")
			assert.False(t, strings.Contains(logged.String(), "/file/bot"), "no log line carries the download link")
		})
	}
}

// withoutLink's two layers: a *url.Error gives way to its cause, so not even
// a link the request was redirected to is left, and a token in the cause's
// own text is cut out; a cancelled download still reads as cancelled.
func TestWithoutLink_CutsTheLinkAndTheToken(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	const token = "999:TOK-zzz"
	link := "https://api.telegram.org/file/bot" + token + "/a.oga"
	for name, err := range map[string]error{
		"redirected": &url.Error{Op: "Get", URL: "https://api.telegram.org/file/bot" + token + "/moved.oga", Err: errors.New("EOF")},
		"in the cause": &url.Error{Op: "Get", URL: link,
			Err: fmt.Errorf("tls: bad certificate for %s", "https://api.telegram.org/file/bot"+token+"/a.oga?x")},
		"cancelled": &url.Error{Op: "Get", URL: link, Err: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			got := withoutLink(err, link, token)
			assert.NotContains(t, got.Error(), token)
			assert.NotContains(t, got.Error(), "/moved.oga", "no link, not even one the request was sent on to")
			var withLink *url.Error
			assert.False(t, errors.As(got, &withLink), "nothing in its chain still holds the link")
			if name == "cancelled" {
				assert.ErrorIs(t, got, context.Canceled)
			}
		})
	}
}

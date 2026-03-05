package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	tgbot "github.com/go-telegram/bot"
)

const (
	elevenLabsTTSURL = "https://api.elevenlabs.io/v1/text-to-speech/"
	elevenLabsSTTURL = "https://api.elevenlabs.io/v1/speech-to-text"
	elevenLabsDefaultModel = "eleven_multilingual_v2"
)

// generateSpeech converts text to an mp3 audio stream via ElevenLabs TTS.
func (b *Bot) generateSpeech(ctx context.Context, text string) (io.Reader, error) {
	model := b.config.ElevenLabsModel
	if model == "" {
		model = elevenLabsDefaultModel
	}
	body, err := json.Marshal(map[string]string{
		"text":     text,
		"model_id": model,
	})
	if err != nil {
		return nil, fmt.Errorf("elevenlabs TTS marshal error: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		elevenLabsTTSURL+b.config.ElevenLabsVoiceID, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs TTS request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", b.config.ElevenLabsAPIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs TTS error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elevenlabs TTS error: status %d: %s", resp.StatusCode, errBody)
	}
	return resp.Body, nil
}

// transcribeVoice downloads a Telegram voice file and transcribes it via ElevenLabs STT.
// Uses a direct multipart HTTP call instead of the SDK wrapper to avoid a bug in the
// ogen-generated encoder: AdditionalFormats (nil slice) is always written as an empty
// string with Content-Type: application/json, which ElevenLabs rejects with 400.
func (b *Bot) transcribeVoice(ctx context.Context, fileID string) (string, error) {
	// 1. Resolve and download the voice file from Telegram.
	fileInfo, err := b.tgBot.GetFile(ctx, &tgbot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("telegram GetFile error: %w", err)
	}
	downloadURL := b.tgBot.FileDownloadLink(fileInfo)
	audioResp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("voice download error: %w", err)
	}
	defer audioResp.Body.Close()

	// 2. Build multipart body with binary audio — bypasses SDK encoding issues.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model_id", "scribe_v1"); err != nil {
		return "", fmt.Errorf("multipart write error: %w", err)
	}
	part, err := mw.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", fmt.Errorf("multipart create file error: %w", err)
	}
	if _, err := io.Copy(part, audioResp.Body); err != nil {
		return "", fmt.Errorf("multipart copy error: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("multipart close error: %w", err)
	}

	// 3. POST to ElevenLabs STT.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		elevenLabsSTTURL, &buf)
	if err != nil {
		return "", fmt.Errorf("create STT request error: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("xi-api-key", b.config.ElevenLabsAPIKey)

	sttResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("elevenlabs STT request error: %w", err)
	}
	defer sttResp.Body.Close()

	if sttResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sttResp.Body)
		return "", fmt.Errorf("elevenlabs STT error: status %d: %s", sttResp.StatusCode, body)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(sttResp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("elevenlabs STT decode error: %w", err)
	}
	return result.Text, nil
}

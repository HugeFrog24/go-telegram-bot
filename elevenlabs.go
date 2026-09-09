package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

const (
	elevenLabsTTSURL       = "https://api.elevenlabs.io/v1/text-to-speech/"
	elevenLabsSTTURL       = "https://api.elevenlabs.io/v1/speech-to-text"
	elevenLabsDefaultModel = "eleven_multilingual_v2"
)

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
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elevenlabs TTS error: status %d: %s", resp.StatusCode, errBody)
	}
	return resp.Body, nil
}

func (b *Bot) transcribeVoice(ctx context.Context, fileID string) (string, error) {
	audioBytes, err := b.downloadTelegramFile(ctx, fileID)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model_id", "scribe_v1"); err != nil {
		return "", fmt.Errorf("multipart write error: %w", err)
	}
	part, err := mw.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", fmt.Errorf("multipart create file error: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(audioBytes)); err != nil {
		return "", fmt.Errorf("multipart copy error: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("multipart close error: %w", err)
	}

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
	defer func() { _ = sttResp.Body.Close() }()

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

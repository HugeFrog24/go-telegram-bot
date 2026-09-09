package main

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkTrailingCacheBreakpoint(t *testing.T) {
	t.Run("marks the final text block of the final turn", func(t *testing.T) {
		msgs := []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("older")),
			anthropic.NewBetaUserMessage(
				anthropic.NewBetaTextBlock("first"),
				anthropic.NewBetaTextBlock("last"),
			),
		}

		markTrailingCacheBreakpoint(msgs)

		last := msgs[1].Content[1].OfText
		require.NotNil(t, last)
		assert.False(t, param.IsOmitted(last.CacheControl),
			"the trailing block carries the breakpoint")

		// Everything earlier stays unmarked: one breakpoint, not one per block.
		assert.True(t, param.IsOmitted(msgs[1].Content[0].OfText.CacheControl))
		assert.True(t, param.IsOmitted(msgs[0].Content[0].OfText.CacheControl))
	})

	t.Run("marks a trailing image block", func(t *testing.T) {
		msgs := []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(
				anthropic.NewBetaImageBlock(anthropic.BetaFileImageSourceParam{FileID: "file_1"}),
			),
		}

		markTrailingCacheBreakpoint(msgs)

		require.NotNil(t, msgs[0].Content[0].OfImage)
		assert.False(t, param.IsOmitted(msgs[0].Content[0].OfImage.CacheControl))
	})

	t.Run("tolerates empty input", func(t *testing.T) {
		assert.NotPanics(t, func() { markTrailingCacheBreakpoint(nil) })
		assert.NotPanics(t, func() {
			markTrailingCacheBreakpoint([]anthropic.BetaMessageParam{{}})
		})
	})
}

func TestPrepareContextMessages_CacheHistoryToggle(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	b, _ := setupBotForTest(t, 123)
	memory := &ChatMemory{
		Messages: []Message{
			{IsUser: true, Text: "hello"},
			{IsUser: false, Text: "hi there"},
		},
		Size: 10,
	}

	t.Run("enabled by default", func(t *testing.T) {
		msgs := b.prepareContextMessages(memory)
		require.Len(t, msgs, 2)
		assert.False(t, param.IsOmitted(msgs[1].Content[0].OfText.CacheControl))
	})

	t.Run("opt-out leaves history unmarked", func(t *testing.T) {
		off := false
		b.config.CacheHistory = &off
		defer func() { b.config.CacheHistory = nil }()

		msgs := b.prepareContextMessages(memory)
		require.Len(t, msgs, 2)
		assert.True(t, param.IsOmitted(msgs[1].Content[0].OfText.CacheControl))
	})
}

func TestValidateConfig_DebounceMs(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	base := func(ms int) *BotConfig {
		return &BotConfig{
			ID:             "b",
			TelegramToken:  "t",
			Model:          "claude-sonnet-4-6",
			MessagePerHour: 1,
			MessagePerDay:  1,
			DebounceMs:     ms,
		}
	}

	cases := []struct {
		name    string
		ms      int
		wantErr bool
	}{
		{"omitted is valid", 0, false},
		{"typical chat window is valid", 2500, false},
		{"at the ceiling is valid", maxDebounceMs, false},
		{"negative is rejected", -1, true},
		{"above the ceiling is rejected", maxDebounceMs + 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(base(tc.ms), map[string]bool{}, map[string]bool{})
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateConfig_Effort(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	base := func(effort string) *BotConfig {
		return &BotConfig{
			ID:             "b",
			TelegramToken:  "t",
			Model:          "claude-sonnet-5",
			MessagePerHour: 1,
			MessagePerDay:  1,
			Effort:         effort,
		}
	}

	cases := []struct {
		name    string
		effort  string
		wantErr bool
	}{
		{"omitted is valid", "", false},
		{"low is valid", "low", false},
		{"medium is valid", "medium", false},
		{"high is valid", "high", false},
		{"xhigh is valid", "xhigh", false},
		{"max is valid", "max", false},
		{"unknown level is rejected", "turbo", true},
		{"wrong case is rejected", "Low", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(base(tc.effort), map[string]bool{}, map[string]bool{})
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

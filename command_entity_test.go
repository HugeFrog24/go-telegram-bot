package main

import (
	"math"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
)

func commandMessage(text string, entities ...models.MessageEntity) *models.Message {
	for i := range entities {
		entities[i].Type = "bot_command"
	}
	return &models.Message{Text: text, Entities: entities}
}

// Telegram counts entity offsets in UTF-16 code units. Out-of-range values,
// however large, never panic (a panic in a handler would take down every bot
// in the process) and never match.
func TestHandledCommand_OutOfRangeEntities(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, entity := range map[string]models.MessageEntity{
		"huge offset and length":  {Offset: 1 << 62, Length: 1 << 62},
		"huge length":             {Offset: 1, Length: math.MaxInt},
		"largest offset":          {Offset: math.MaxInt, Length: 1},
		"negative offset":         {Offset: -1, Length: 6},
		"negative length":         {Offset: 0, Length: -6},
		"zero length":             {Offset: 0, Length: 0},
		"runs past the end":       {Offset: 1, Length: 6},
		"starts at the end":       {Offset: 6, Length: 1},
		"starts inside a surrogate pair": {Offset: 1, Length: 7},
		"ends inside a surrogate pair":   {Offset: 0, Length: 7},
	} {
		t.Run(name, func(t *testing.T) {
			text := map[string]string{
				"starts inside a surrogate pair": "😀/clear",
				"ends inside a surrogate pair":   "/clear😀",
			}[name]
			if text == "" {
				text = "/clear"
			}
			assert.NotPanics(t, func() {
				assert.Empty(t, handledCommand(commandMessage(text, entity)))
			})
		})
	}
}

// An overlapping, out-of-range entity before a good one is skipped.
func TestHandledCommand_OverlappingEntities(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	assert.NotPanics(t, func() {
		assert.Equal(t, "/stats", handledCommand(commandMessage("/stats /clear",
			models.MessageEntity{Offset: 3, Length: 1 << 40},
			models.MessageEntity{Offset: 2, Length: 6},
			models.MessageEntity{Offset: 0, Length: 6},
		)))
	})
}

// Text before the command that takes more bytes than UTF-16 units: the
// offset is converted, not used as a byte index.
func TestHandledCommand_UTF16Offsets(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	for name, tc := range map[string]struct {
		text           string
		offset, length int
	}{
		"Cyrillic":       {"привет /clear", 7, 6}, // 2 bytes, 1 unit per letter
		"emoji":          {"😀 /clear", 3, 6},      // 4 bytes, 2 units
		"emoji and text": {"hi 👋🏽 /stats user", 8, 6},
	} {
		t.Run(name, func(t *testing.T) {
			want := tc.text[len(tc.text)-len("/clear"):]
			if name == "emoji and text" {
				want = "/stats"
			}
			assert.Equal(t, want, handledCommand(commandMessage(tc.text,
				models.MessageEntity{Offset: tc.offset, Length: tc.length})))
		})
	}
}

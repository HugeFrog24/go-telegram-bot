package main

import (
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
)

func TestLargestPhotoSize(t *testing.T) {
	cases := []struct {
		name       string
		photos     []models.PhotoSize
		wantFileID string
	}{
		{
			name: "ascending sizes — last is largest",
			photos: []models.PhotoSize{
				{FileID: "thumb", Width: 90, Height: 90},
				{FileID: "small", Width: 320, Height: 320},
				{FileID: "full", Width: 1280, Height: 720},
			},
			wantFileID: "full",
		},
		{
			name: "descending sizes — first is largest",
			photos: []models.PhotoSize{
				{FileID: "full", Width: 1280, Height: 720},
				{FileID: "small", Width: 320, Height: 320},
				{FileID: "thumb", Width: 90, Height: 90},
			},
			wantFileID: "full",
		},
		{
			name: "single photo",
			photos: []models.PhotoSize{
				{FileID: "solo", Width: 800, Height: 600},
			},
			wantFileID: "solo",
		},
		{
			name:       "empty slice returns zero value (caller guards upstream)",
			photos:     []models.PhotoSize{},
			wantFileID: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := largestPhotoSize(tc.photos)
			assert.Equal(t, tc.wantFileID, got.FileID)
		})
	}
}

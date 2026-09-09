package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/sync/errgroup"
)

func (b *Bot) handleVoiceMessage(ctx context.Context, message *models.Message, userMsg Message, chatID, userID int64, username, firstName, lastName string, isPremium bool, languageCode string, messageTime int, businessConnectionID string) {
	if b.config.ElevenLabsAPIKey == "" {
		if err := b.sendResponse(ctx, chatID, "I don't understand voice messages.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending voice-unsupported message: %v", err)
		}
		return
	}

	if !b.hasScope(userID, ScopeTTSUse) {
		if err := b.sendResponse(ctx, chatID, "You don't have permission to use voice features.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending permission denied message: %v", err)
		}
		return
	}

	// Voice is a committed turn: it waits for any running turn and is never
	// restarted, so a text sent right after a voice note is answered afterwards.
	turn, turnCtx := b.beginTurn(ctx, chatID)
	defer b.endTurn(turn)
	ctx = turnCtx

	stopTyping := b.startChatAction(ctx, chatID, businessConnectionID, models.ChatActionTyping)
	defer stopTyping()

	transcript, err := b.transcribeVoice(ctx, message.Voice.FileID)
	if err != nil {
		ErrorLogger.Printf("Error transcribing voice message from user %d: %v", userID, err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't understand your voice message.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending transcription error message: %v", err)
		}
		return
	}

	if err := b.db.Model(&userMsg).Update("text", transcript).Error; err != nil {
		ErrorLogger.Printf("Error updating voice transcript in DB: %v", err)
	}
	b.chatMemoriesMu.Lock()
	if mem, exists := b.chatMemories[chatID]; exists {
		for i := len(mem.Messages) - 1; i >= 0; i-- {
			if mem.Messages[i].ID == userMsg.ID {
				mem.Messages[i].Text = transcript
				break
			}
		}
	}
	b.chatMemoriesMu.Unlock()

	contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
	response, err := b.getAnthropicResponse(ctx, chatID, contextMessages, false, username, firstName, lastName, isPremium, languageCode, messageTime, nil, nil)
	if err != nil {
		ErrorLogger.Printf("Error getting Anthropic response for voice: %v", err)
		if err := b.sendResponse(ctx, chatID, b.anthropicErrorResponse(err, userID), businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending anthropic error response: %v", err)
		}
		return
	}

	// Switch the indicator once the model is done and synthesis begins, so the
	// client shows "recording audio" rather than "typing" for a voice reply.
	stopTyping()
	stopRecording := b.startChatAction(ctx, chatID, businessConnectionID, models.ChatActionUploadVoice)
	defer stopRecording()

	audioReader, err := b.generateSpeech(ctx, response)
	if err != nil {
		ErrorLogger.Printf("Error generating speech, falling back to text: %v", err)
		if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending text fallback: %v", err)
		}
		return
	}

	if _, err := b.screenOutgoingMessage(chatID, response); err != nil {
		ErrorLogger.Printf("Error storing assistant voice response: %v", err)
	}

	params := &bot.SendAudioParams{
		ChatID: chatID,
		Audio:  &models.InputFileUpload{Filename: "response.mp3", Data: audioReader},
	}
	if businessConnectionID != "" {
		params.BusinessConnectionID = businessConnectionID
	}
	if _, err := b.tgBot.SendAudio(ctx, params); err != nil {
		ErrorLogger.Printf("Error sending audio to chat %d: %v", chatID, err)
	}
}

func (b *Bot) uploadPhotoFromItem(ctx context.Context, item *models.Message, chatID int64) (string, error) {
	photo := largestPhotoSize(item.Photo)
	data, err := b.downloadTelegramFile(ctx, photo.FileID)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", photo.FileID, err)
	}
	filename := formatUploadFilename(b.botID, chatID, item.ID, "jpg")
	return b.uploadImageToAnthropic(ctx, data, filename, "image/jpeg")
}

func (b *Bot) handlePhotoMessage(
	ctx context.Context,
	items []*models.Message,
	chatID, userID int64,
	username, firstName, lastName string,
	isPremium bool,
	languageCode string,
	messageTime int,
	businessConnectionID string,
) {
	if len(items) == 0 {
		return
	}

	// The Files API uploads depend on nothing the chat's running turn is doing,
	// so they overlap it instead of queueing behind it. On an album this is the
	// longest wait in the bot, and the typing indicator covers it.
	stopTyping := b.startChatAction(ctx, chatID, businessConnectionID, models.ChatActionTyping)
	finalUploaded, caption, err := b.uploadPhotoItems(ctx, items, chatID, businessConnectionID)
	stopTyping()
	if err != nil || len(finalUploaded) == 0 {
		return
	}

	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: userID,
		username: username, firstName: firstName, lastName: lastName,
		languageCode: languageCode, isPremium: isPremium, messageTime: messageTime,
		businessConnectionID: businessConnectionID,
	}
	// Photos wait for any running turn and are never restarted.
	turn, turnCtx := b.beginTurn(ctx, chatID)

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		chatMemory := b.getOrCreateChatMemory(chatID)
		userMessage := b.createMessage(chatID, userID, username, "user", caption, true)
		userMessage.ImageFileIDs = finalUploaded
		userMessage.TelegramMessageID = items[0].ID
		if err := b.storeMessage(&userMessage); err != nil {
			b.compensatingDelete(ctx, finalUploaded)
			ErrorLogger.Printf("[%s] store photo message failed: %v", b.config.ID, err)
			if sendErr := b.sendResponse(ctx, chatID, "Sorry, I had trouble saving your message.", businessConnectionID); sendErr != nil {
				ErrorLogger.Printf("Error sending store failure message: %v", sendErr)
			}
			return "", errTurnSilent
		}
		b.addMessageToChatMemory(chatMemory, userMessage)

		contextMessages := b.snapshotForTurn(turn, chatMemory)
		return b.getAnthropicResponse(
			ctx, chatID, contextMessages, false,
			username, firstName, lastName, isPremium, languageCode, messageTime,
			out.onSegment, out.onProgress,
		)
	})
}

// uploadPhotoItems uploads every photo in items to the Files API in parallel
// and returns their file ids with the album caption. On any failure it deletes
// the uploads that did succeed, tells the user, and returns errTurnSilent.
func (b *Bot) uploadPhotoItems(ctx context.Context, items []*models.Message, chatID int64, businessConnectionID string) ([]string, string, error) {
	uploaded := make([]string, len(items))
	caption := ""
	g, gctx := errgroup.WithContext(ctx)
	for i, item := range items {
		if item.Caption != "" {
			caption = item.Caption
		}
		if len(item.Photo) == 0 {
			continue
		}
		i, item := i, item
		g.Go(func() error {
			fileID, err := b.uploadPhotoFromItem(gctx, item, chatID)
			if err != nil {
				return err
			}
			uploaded[i] = fileID
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		ErrorLogger.Printf("[%s] photo upload failed: %v", b.config.ID, err)
		var successful []string
		for _, fid := range uploaded {
			if fid != "" {
				successful = append(successful, fid)
			}
		}
		b.compensatingDelete(ctx, successful)
		if sendErr := b.sendResponse(ctx, chatID, "Sorry, I couldn't process one of your images.", businessConnectionID); sendErr != nil {
			ErrorLogger.Printf("Error sending photo failure message: %v", sendErr)
		}
		return nil, "", errTurnSilent
	}
	finalUploaded := make([]string, 0, len(uploaded))
	for _, fid := range uploaded {
		if fid != "" {
			finalUploaded = append(finalUploaded, fid)
		}
	}
	return finalUploaded, caption, nil
}

// respondToChat runs one assistant turn against the chat's current memory and
// streams the reply back. Both the immediate path and the debounced flush go
// through here, so a coalesced turn is byte-for-byte the same request as a
// single-message one: the messages were already written to memory at intake, and
// the model simply sees more of them.
//
// If a turn is already running for the chat, beginTextTurn decides whether this
// flush is dropped, restarts it, or runs after it; see turn.go.
func (b *Bot) respondToChat(req turnRequest) {
	wantID := b.newestUserMessageIDInChat(req.chatID)
	if wantID == 0 {
		// Nothing of the user's is in memory, for example because /clear
		// landed after this flush was collected. There is nothing to answer.
		InfoLogger.Printf("[%s] turn: chat %d has no user message to answer; skipping", b.config.ID, req.chatID)
		return
	}
	turn, turnCtx, ok := b.beginTextTurn(req, wantID)
	if !ok {
		return
	}

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(req.chatID))
		return b.getAnthropicResponse(
			ctx, req.chatID, contextMessages, req.isEmojiOnly,
			req.username, req.firstName, req.lastName, req.isPremium, req.languageCode, req.messageTime,
			out.onSegment, out.onProgress,
		)
	})
}

func (b *Bot) anthropicErrorResponse(err error, userID int64) string {
	isElevated := b.hasScope(userID, ScopeModelSet)

	if errors.Is(err, ErrModelNotFound) && isElevated {
		return fmt.Sprintf(
			"⚠️ Model `%s` is no longer available (deprecated or removed by Anthropic).\n"+
				"Use /set_model <model-id> to switch. Current models: https://platform.claude.com/docs/en/about-claude/models/overview",
			b.config.Model,
		)
	}

	if isElevated {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			body := apiErr.RawJSON()
			if len(body) > 800 {
				body = body[:800] + "...(truncated)"
			}
			out := fmt.Sprintf("⚠️ Anthropic API error %d:\n%s", apiErr.StatusCode, body)
			if apiErr.RequestID != "" {
				out += fmt.Sprintf("\nRequest-ID: %s", apiErr.RequestID)
			}
			return out
		}
		return fmt.Sprintf("⚠️ Anthropic call failed: %v", err)
	}

	return "I'm sorry, I'm having trouble processing your request right now."
}

func (b *Bot) handleUpdate(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if stopped := update.StoppedMessageGeneration; stopped != nil {
		b.handleStopGeneration(stopped.Chat.ID, stopped.DraftID)
		return
	}
	if edited := update.EditedMessage; edited != nil {
		b.handleEditedMessage(ctx, edited)
		return
	}
	if edited := update.EditedBusinessMessage; edited != nil {
		b.handleEditedMessage(ctx, edited)
		return
	}

	var message *models.Message

	if update.Message != nil {
		message = update.Message
	} else if update.BusinessMessage != nil {
		message = update.BusinessMessage
	} else {
		return
	}

	var businessConnectionID string
	if update.BusinessConnection != nil {
		businessConnectionID = update.BusinessConnection.ID
	} else if message.BusinessConnectionID != "" {
		businessConnectionID = message.BusinessConnectionID
	}

	if message.From == nil {
		return
	}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := message.From.Username
	firstName := message.From.FirstName
	lastName := message.From.LastName
	languageCode := message.From.LanguageCode
	isPremium := message.From.IsPremium
	messageTime := message.Date
	text := message.Text

	var isOwner bool
	if b.db.Where("telegram_id = ? AND bot_id = ? AND is_owner = ?", userID, b.botID, true).First(&User{}).Error == nil {
		isOwner = true
	}

	user, err := b.getOrCreateUser(userID, username, isOwner)
	if err != nil {
		ErrorLogger.Printf("Error getting or creating user: %v", err)
		return
	}

	if user.Username != username {
		user.Username = username
		if err := b.db.Save(&user).Error; err != nil {
			ErrorLogger.Printf("Error updating user username: %v", err)
		}
	}

	// Media never waits on the text debounce window. Cancelling here does not
	// discard the buffered text: those messages are already in chat memory, so
	// the turn this media triggers answers them too.
	if message.MediaGroupID != "" && len(message.Photo) > 0 {
		b.cancelIntake(chatID)
		b.bufferAlbumItem(ctx, message, chatID, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime, businessConnectionID)
		return
	}
	if len(message.Photo) > 0 {
		b.cancelIntake(chatID)
		if !b.checkRateLimits(userID) {
			b.sendRateLimitExceededMessage(ctx, chatID, businessConnectionID)
			return
		}
		b.handlePhotoMessage(ctx, []*models.Message{message},
			chatID, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime,
			businessConnectionID)
		return
	}

	userMsg, err := b.screenIncomingMessage(message)
	if err != nil {
		ErrorLogger.Printf("Error storing user message: %v", err)
		return
	}

	if message.Entities != nil {
		for _, entity := range message.Entities {
			if entity.Type == "bot_command" {
				command := strings.TrimSpace(message.Text[entity.Offset : entity.Offset+entity.Length])
				switch command {
				case "/stats":
					parts := strings.Fields(message.Text)

					if len(parts) == 1 {
						b.sendStats(ctx, chatID, userID, 0, businessConnectionID)
						return
					}

					if len(parts) >= 2 && parts[1] == "user" {
						targetUserID := userID

						if len(parts) >= 3 {
							var parseErr error
							targetUserID, parseErr = strconv.ParseInt(parts[2], 10, 64)
							if parseErr != nil {
								InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[2])
								if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /stats user [user_id]", businessConnectionID); err != nil {
									ErrorLogger.Printf("Error sending response: %v", err)
								}
								return
							}
						}

						b.sendStats(ctx, chatID, userID, targetUserID, businessConnectionID)
						return
					}

					if err := b.sendResponse(ctx, chatID, "Invalid command format. Usage: /stats or /stats user [user_id]", businessConnectionID); err != nil {
						ErrorLogger.Printf("Error sending response: %v", err)
					}
					return
				case "/whoami":
					b.sendWhoAmI(ctx, chatID, userID, username, businessConnectionID)
					return
				case "/clear":
					parts := strings.Fields(message.Text)
					var targetUserID, targetChatID int64
					if len(parts) > 1 {
						var parseErr error
						targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
							if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					if len(parts) > 2 {
						var parseErr error
						targetChatID, parseErr = strconv.ParseInt(parts[2], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid chat ID format: %s", userID, parts[2])
							if err := b.sendResponse(ctx, chatID, "Invalid chat ID format. Usage: /clear [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					b.clearChatHistory(ctx, chatID, userID, targetUserID, targetChatID, businessConnectionID, false)
					return
				case "/set_model":
					if !b.hasScope(userID, ScopeModelSet) {
						if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can change the model.", businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					parts := strings.Fields(message.Text)
					if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
						if err := b.sendResponse(ctx, chatID, "Usage: /set_model <model-id>", businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					newModel := strings.TrimSpace(parts[1])
					if err := b.config.PersistModel(newModel); err != nil {
						ErrorLogger.Printf("Failed to persist model change: %v", err)
						if err := b.sendResponse(ctx, chatID, fmt.Sprintf("Model updated in memory to `%s`, but failed to save to config file: %v", newModel, err), businessConnectionID); err != nil {
							ErrorLogger.Printf("Error sending response: %v", err)
						}
						return
					}
					InfoLogger.Printf("Model changed to %s by user %d", newModel, userID)
					if err := b.sendResponse(ctx, chatID, fmt.Sprintf("✅ Model updated to `%s` and saved to config.", newModel), businessConnectionID); err != nil {
						ErrorLogger.Printf("Error sending response: %v", err)
					}
					return
				case "/clear_hard":
					parts := strings.Fields(message.Text)
					var targetUserID, targetChatID int64
					if len(parts) > 1 {
						var parseErr error
						targetUserID, parseErr = strconv.ParseInt(parts[1], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[1])
							if err := b.sendResponse(ctx, chatID, "Invalid user ID format. Usage: /clear_hard [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					if len(parts) > 2 {
						var parseErr error
						targetChatID, parseErr = strconv.ParseInt(parts[2], 10, 64)
						if parseErr != nil {
							InfoLogger.Printf("User %d provided invalid chat ID format: %s", userID, parts[2])
							if err := b.sendResponse(ctx, chatID, "Invalid chat ID format. Usage: /clear_hard [user_id] [chat_id]", businessConnectionID); err != nil {
								ErrorLogger.Printf("Error sending response: %v", err)
							}
							return
						}
					}
					b.clearChatHistory(ctx, chatID, userID, targetUserID, targetChatID, businessConnectionID, true)
					return
				}
			}
		}
	}

	if !b.checkRateLimits(userID) {
		b.sendRateLimitExceededMessage(ctx, chatID, businessConnectionID)
		return
	}

	if message.Voice != nil {
		b.cancelIntake(chatID)
		b.handleVoiceMessage(ctx, message, userMsg, chatID, userID, username, firstName, lastName, isPremium, languageCode, messageTime, businessConnectionID)
		return
	}

	if message.Sticker != nil {
		b.cancelIntake(chatID)
		turn, turnCtx := b.beginTurn(ctx, chatID)
		defer b.endTurn(turn)
		contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(chatID))
		b.handleStickerMessage(turnCtx, chatID, userMsg, message, contextMessages, businessConnectionID)
		return
	}

	if text == "" {
		InfoLogger.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
		return
	}

	isEmojiOnly := isOnlyEmojis(text)

	// Plain text is the only thing that debounces: it is what users fragment
	// across several sends, and it is the only kind whose meaning survives being
	// read as one turn.
	if b.config.DebounceWindow() > 0 {
		b.bufferIntake(ctx, chatID, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime, businessConnectionID, isEmojiOnly)
		return
	}

	b.respondToChat(turnRequest{
		ctx: ctx, chatID: chatID, userID: userID, isEmojiOnly: isEmojiOnly,
		username: username, firstName: firstName, lastName: lastName,
		isPremium: isPremium, languageCode: languageCode, messageTime: messageTime,
		businessConnectionID: businessConnectionID,
	})
}

// handleEditedMessage applies a Telegram edit to the stored copy of a user
// message. Mobile clients deliver autocorrect fixes as edits within a few
// seconds of the send, so the model should see the corrected text. What
// happens next depends on where the message is:
//
//   - still in its quiet window: the window restarts as if it had just arrived;
//   - being read by a turn that has shown nothing yet: that turn restarts;
//   - already answered, or shown: only the record is corrected.
func (b *Bot) handleEditedMessage(ctx context.Context, edited *models.Message) {
	text := edited.Text
	if text == "" {
		text = edited.Caption
	}
	if edited.From == nil || text == "" {
		return
	}
	chatID := edited.Chat.ID

	// Memory is the gate: only a message still in memory can influence a turn,
	// and the check is cheap. It also keeps edits from chats this bot never
	// stored off the database; a business connection delivers every edit on
	// the whole account.
	var msgID uint
	answered := false
	b.chatMemoriesMu.Lock()
	if mem, exists := b.chatMemories[chatID]; exists {
		for i := len(mem.Messages) - 1; i >= 0; i-- {
			m := &mem.Messages[i]
			if m.IsUser && m.TelegramMessageID == edited.ID {
				m.Text = text
				msgID = m.ID
				break
			}
			if !m.IsUser {
				answered = true
			}
		}
	}
	b.chatMemoriesMu.Unlock()
	if msgID == 0 {
		return
	}

	if err := b.db.Model(&Message{}).Where("id = ?", msgID).Update("text", text).Error; err != nil {
		ErrorLogger.Printf("[%s] applying edit to message %d in chat %d: %v", b.config.ID, edited.ID, chatID, err)
	}
	if answered {
		return
	}

	isEmojiOnly := isOnlyEmojis(text)
	if b.touchIntake(chatID, isEmojiOnly) {
		InfoLogger.Printf("[%s] edit to message %d re-armed the quiet window for chat %d", b.config.ID, edited.ID, chatID)
		return
	}

	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: edited.From.ID,
		username: edited.From.Username, firstName: edited.From.FirstName, lastName: edited.From.LastName,
		languageCode: edited.From.LanguageCode, isPremium: edited.From.IsPremium, messageTime: edited.Date,
		businessConnectionID: edited.BusinessConnectionID, isEmojiOnly: isEmojiOnly,
	}
	if b.restartForEdit(req, msgID) {
		InfoLogger.Printf("[%s] edit to message %d restarted the running turn for chat %d", b.config.ID, edited.ID, chatID)
	}
}

// handleStopGeneration routes Telegram's stopped_message_generation update,
// sent when the user taps Stop under a streaming draft.
func (b *Bot) handleStopGeneration(chatID int64, draftID int) {
	if b.stopTurn(chatID, draftID) {
		InfoLogger.Printf("[%s] user stopped generation in chat %d (draft %d)", b.config.ID, chatID, draftID)
		return
	}
	InfoLogger.Printf("[%s] stop for chat %d ignored: draft %d is not the running turn", b.config.ID, chatID, draftID)
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, chatID int64, businessConnectionID string) {
	if err := b.sendResponse(ctx, chatID, "Rate limit exceeded. Please try again later.", businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending rate limit exceeded message: %v", err)
	}
}

func (b *Bot) handleStickerMessage(ctx context.Context, chatID int64, userMessage Message, message *models.Message, contextMessages []anthropic.BetaMessageParam, businessConnectionID string) {

	response, err := b.generateStickerResponse(ctx, userMessage, contextMessages, businessConnectionID)
	if err != nil {
		ErrorLogger.Printf("Error generating sticker response: %v", err)
		if message.Sticker.IsAnimated {
			response = "Wow, that's a cool animated sticker!"
		} else if message.Sticker.IsVideo {
			response = "Interesting video sticker!"
		} else {
			response = "That's a cool sticker!"
		}
	}

	if err := b.sendResponse(ctx, chatID, response, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
		return
	}
}

func (b *Bot) generateStickerResponse(ctx context.Context, message Message, contextMessages []anthropic.BetaMessageParam, businessConnectionID string) (string, error) {
	stopTyping := b.startChatAction(ctx, message.ChatID, businessConnectionID, models.ChatActionTyping)
	defer stopTyping()

	if message.StickerFileID != "" {
		messageTime := int(message.Timestamp.Unix())
		response, err := b.getAnthropicResponse(ctx, message.ChatID, contextMessages, true, message.Username, "", "", false, "", messageTime, nil, nil)
		if err != nil {
			return "", err
		}
		return response, nil
	}

	return "Hmm, that's interesting!", nil
}

func (b *Bot) clearChatHistory(ctx context.Context, chatID int64, currentUserID int64, targetUserID int64, targetChatID int64, businessConnectionID string, hardDelete bool) {
	if targetUserID != 0 && targetUserID != currentUserID {
		requiredScope := ScopeHistoryClearAny
		if hardDelete {
			requiredScope = ScopeHistoryClearHardAny
		}
		if !b.hasScope(currentUserID, requiredScope) {
			InfoLogger.Printf("User %d attempted to clear history for user %d without permission", currentUserID, targetUserID)
			if err := b.sendResponse(ctx, chatID, "Permission denied. Only admins and owners can clear other users' histories.", businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}

		var targetUser User
		err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error
		if err != nil {
			ErrorLogger.Printf("Error finding target user %d: %v", targetUserID, err)
			if err := b.sendResponse(ctx, chatID, fmt.Sprintf("User with ID %d not found.", targetUserID), businessConnectionID); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}
	} else {
		targetUserID = currentUserID
	}

	var err error
	if hardDelete {
		if targetUserID == currentUserID {
			err = b.hardDeleteScope(ctx, "chat_id = ? AND bot_id = ?", chatID, b.botID)
			InfoLogger.Printf("User %d permanently deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			if targetChatID != 0 {
				err = b.hardDeleteScope(ctx, "chat_id = ? AND bot_id = ?", targetChatID, b.botID)
				InfoLogger.Printf("Admin/owner %d permanently deleted chat history for user %d in chat %d", currentUserID, targetUserID, targetChatID)
			} else {
				err = b.hardDeleteScope(ctx,
					"bot_id = ? AND (user_id = ? OR (chat_id = ? AND is_user = ?))",
					b.botID, targetUserID, targetUserID, false)
				InfoLogger.Printf("Admin/owner %d permanently deleted all chat history for user %d", currentUserID, targetUserID)
			}
		}
	} else {
		if targetUserID == currentUserID {
			err = b.db.Where("chat_id = ? AND bot_id = ?", chatID, b.botID).Delete(&Message{}).Error
			InfoLogger.Printf("User %d soft deleted their own chat history in chat %d", currentUserID, chatID)
		} else {
			if targetChatID != 0 {
				err = b.db.Where("chat_id = ? AND bot_id = ?", targetChatID, b.botID).Delete(&Message{}).Error
				InfoLogger.Printf("Admin/owner %d soft deleted chat history for user %d in chat %d", currentUserID, targetUserID, targetChatID)
			} else {
				err = b.db.Where("bot_id = ? AND user_id = ?", b.botID, targetUserID).Delete(&Message{}).Error
				if err == nil {
					err = b.db.Where("chat_id = ? AND bot_id = ? AND is_user = ?", targetUserID, b.botID, false).Delete(&Message{}).Error
				}
				InfoLogger.Printf("Admin/owner %d soft deleted all chat history for user %d", currentUserID, targetUserID)
			}
		}
	}

	if err != nil {
		ErrorLogger.Printf("Error clearing chat history: %v", err)
		if err := b.sendResponse(ctx, chatID, "Sorry, I couldn't clear the chat history.", businessConnectionID); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	// Drop any armed intake buffer for the same chat before clearing memory.
	// Otherwise the debounce timer fires moments later and repopulates the chat
	// with the very messages that were just deleted — the openclaw/openclaw#51046
	// failure mode, but with a privacy consequence rather than a stray reply.
	clearedChatID := chatID
	if targetUserID != currentUserID {
		clearedChatID = targetChatID
		if clearedChatID == 0 {
			clearedChatID = targetUserID
		}
	}
	if discarded := b.cancelIntake(clearedChatID); discarded > 0 {
		InfoLogger.Printf("[%s] discarded %d buffered message(s) for chat %d on history clear",
			b.config.ID, discarded, clearedChatID)
	}
	// Likewise a flush collected behind a running turn: rerun against the
	// emptied chat, it would call the model with no user message at all.
	b.discardRerun(clearedChatID)

	b.chatMemoriesMu.Lock()
	if targetUserID == currentUserID {
		delete(b.chatMemories, chatID)
	} else if targetChatID != 0 {
		delete(b.chatMemories, targetChatID)
	} else {
		delete(b.chatMemories, targetUserID)
	}
	b.chatMemoriesMu.Unlock()

	var confirmationMessage string
	if targetUserID == currentUserID {
		confirmationMessage = "Your chat history has been cleared."
	} else {
		var targetUser User
		err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error
		if err == nil && targetUser.Username != "" {
			confirmationMessage = fmt.Sprintf("Chat history for user @%s (ID: %d) has been cleared.", targetUser.Username, targetUserID)
		} else {
			confirmationMessage = fmt.Sprintf("Chat history for user with ID %d has been cleared.", targetUserID)
		}
	}

	if err := b.sendResponse(ctx, chatID, confirmationMessage, businessConnectionID); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/sync/errgroup"
)

// handleVoiceMessage answers a voice note as its own committed turn. pending
// is the text batch the note took over from the quiet window, or nil.
func (b *Bot) handleVoiceMessage(ctx context.Context, message *models.Message, userMsg Message, pending *turnRequest, to replyTarget, userID int64, username, firstName, lastName string, isPremium bool, languageCode string, messageTime int) {
	if b.config.ElevenLabsAPIKey == "" {
		b.refuseVoice(ctx, &userMsg, pending, to, "I don't understand voice messages.")
		return
	}

	if !b.hasScope(userID, ScopeTTSUse) {
		b.refuseVoice(ctx, &userMsg, pending, to, "You don't have permission to use voice features.")
		return
	}

	// Voice is a committed turn: it waits for any running turn and is never
	// restarted, so a text sent right after a voice note is answered afterwards.
	deliverCtx := ctx // for the status message: ending the turn does not cancel it
	turn, turnCtx := b.beginTurn(ctx, to.chatID)
	defer b.endTurn(turn)
	ctx = turnCtx

	stopTyping := b.startChatAction(ctx, to, models.ChatActionTyping)
	defer stopTyping()

	transcript, err := b.transcribeVoice(ctx, message.Voice.FileID)
	if err == nil && strings.TrimSpace(transcript) == "" {
		err = errors.New("empty transcript")
	}
	if err != nil {
		ErrorLogger.Printf("Error transcribing voice message from user %d: %v", userID, err)
		b.refuseVoice(ctx, &userMsg, pending, to, "Sorry, I couldn't understand your voice message.")
		return
	}

	// The note joins the conversation only now, as its transcript: until then
	// no other turn could read its placeholder, cover it, or answer it.
	userMsg.Text = transcript
	if err := b.db.Model(&Message{}).Where("id = ?", userMsg.ID).Update("text", transcript).Error; err != nil {
		ErrorLogger.Printf("Error updating voice transcript in DB: %v", err)
	}
	b.joinConversation(userMsg)

	// Notes show while the model works. Once it is done, notes end: one still
	// waiting is dropped, so none first appears during speech synthesis. The
	// deferred close removes a status message already shown once the audio,
	// or its text fallback, is out.
	var sinks replySinks
	freezeStatus, closeStatus := noStatus, noStatus
	if status := b.startStatusLine(turnCtx, deliverCtx, to); status != nil {
		sinks.onNote = status.note
		freezeStatus, closeStatus = status.freeze, status.close
	}
	defer closeStatus()

	contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(to.chatID))
	response, err := b.getAnthropicResponse(ctx, to.chatID, contextMessages, false, username, firstName, lastName, isPremium, languageCode, messageTime, sinks)
	freezeStatus()
	if err != nil {
		ErrorLogger.Printf("Error getting Anthropic response for voice: %v", err)
		closeStatus()
		if err := b.sendTurnReply(ctx, turn, to, b.anthropicErrorResponse(err, userID)); err != nil {
			ErrorLogger.Printf("Error sending anthropic error response: %v", err)
		}
		return
	}

	// Switch the indicator once the model is done and synthesis begins, so the
	// client shows "recording audio" rather than "typing" for a voice reply.
	stopTyping()
	stopRecording := b.startChatAction(ctx, to, models.ChatActionUploadVoice)
	defer stopRecording()

	audioReader, err := b.generateSpeech(ctx, response)
	if err != nil {
		ErrorLogger.Printf("Error generating speech, falling back to text: %v", err)
		if err := b.sendTurnReply(ctx, turn, to, response); err != nil {
			ErrorLogger.Printf("Error sending text fallback: %v", err)
		}
		return
	}

	if _, err := b.recordTurnReply(turn, to.chatID, response); err != nil {
		ErrorLogger.Printf("Error storing assistant voice response: %v", err)
	}

	params := &bot.SendAudioParams{
		ChatID:               to.chatID,
		MessageThreadID:      to.threadID,
		BusinessConnectionID: to.businessConnectionID,
		Audio:                &models.InputFileUpload{Filename: "response.mp3", Data: audioReader},
	}
	if _, err := b.tgBot.SendAudio(ctx, params); err != nil {
		ErrorLogger.Printf("Error sending audio to chat %d: %v", to.chatID, err)
	}
}

// refuseVoice sets a voice note that will not be transcribed aside, with the
// notice that tells the user why, and answers the text batch it took over.
func (b *Bot) refuseVoice(ctx context.Context, userMsg *Message, pending *turnRequest, to replyTarget, notice string) {
	b.setAside(userMsg)
	if err := b.sendResponse(ctx, to, notice); err != nil {
		ErrorLogger.Printf("Error sending voice notice: %v", err)
	}
	b.answerPending(pending)
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

// handlePhotoMessage answers a photo or an album as its own committed turn.
// pending is the text batch the photos took over from the quiet window, or
// nil; it is answered on its own when no photo turn reaches the model.
func (b *Bot) handlePhotoMessage(
	ctx context.Context,
	items []*models.Message,
	pending *turnRequest,
	to replyTarget,
	userID int64,
	username, firstName, lastName string,
	isPremium bool,
	languageCode string,
	messageTime int,
) {
	if len(items) == 0 {
		return
	}

	// The Files API uploads depend on nothing the chat's running turn is doing,
	// so they overlap it instead of queueing behind it. On an album this is the
	// longest wait in the bot, and the typing indicator covers it.
	stopTyping := b.startChatAction(ctx, to, models.ChatActionTyping)
	finalUploaded, caption, err := b.uploadPhotoItems(ctx, items, to)
	stopTyping()
	if err != nil || len(finalUploaded) == 0 {
		b.answerPending(pending)
		return
	}

	req := turnRequest{
		ctx: ctx, chatID: to.chatID, userID: userID,
		username: username, firstName: firstName, lastName: lastName,
		languageCode: languageCode, isPremium: isPremium, messageTime: messageTime,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID,
	}
	// Photos wait for any running turn and are never restarted.
	turn, turnCtx := b.beginTurn(ctx, to.chatID)

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		chatMemory := b.getOrCreateChatMemory(to.chatID)
		userMessage := b.createMessage(to.chatID, userID, username, "user", caption, true)
		userMessage.ImageFileIDs = finalUploaded
		userMessage.TelegramMessageID = items[0].ID
		if err := b.storeMessage(&userMessage); err != nil {
			b.compensatingDelete(ctx, finalUploaded)
			ErrorLogger.Printf("[%s] store photo message failed: %v", b.config.ID, err)
			if sendErr := b.sendResponse(ctx, to, "Sorry, I had trouble saving your message."); sendErr != nil {
				ErrorLogger.Printf("Error sending store failure message: %v", sendErr)
			}
			b.answerPending(pending) // collected: runs once this turn ends
			return "", errTurnSilent
		}
		b.joinConversation(userMessage)

		contextMessages := b.snapshotForTurn(turn, chatMemory)
		return b.getAnthropicResponse(
			ctx, to.chatID, contextMessages, false,
			username, firstName, lastName, isPremium, languageCode, messageTime,
			out.sinks(),
		)
	})
}

// uploadPhotoItems uploads every photo in items to the Files API in parallel
// and returns their file ids with the album caption. On any failure it deletes
// the uploads that did succeed, tells the user, and returns errTurnSilent.
func (b *Bot) uploadPhotoItems(ctx context.Context, items []*models.Message, to replyTarget) ([]string, string, error) {
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
			fileID, err := b.uploadPhotoFromItem(gctx, item, to.chatID)
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
		if sendErr := b.sendResponse(ctx, to, "Sorry, I couldn't process one of your images."); sendErr != nil {
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
// single-message one: the messages joined memory before they were buffered, and
// the model simply sees more of them.
//
// If a turn is already running for the chat, beginTextTurn decides whether this
// flush is dropped, restarts it, or runs after it; see turn.go.
func (b *Bot) respondToChat(req turnRequest) {
	wantSeq := b.newestAnswerableSeqInChat(req.chatID)
	if wantSeq == 0 {
		// Nothing of the user's is in memory, for example because /clear
		// landed after this flush was collected. There is nothing to answer.
		InfoLogger.Printf("[%s] turn: chat %d has no user message to answer; skipping", b.config.ID, req.chatID)
		return
	}
	turn, turnCtx, ok := b.beginTextTurn(req, wantSeq)
	if !ok {
		return
	}

	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(req.chatID))
		return b.getAnthropicResponse(
			ctx, req.chatID, contextMessages, req.isEmojiOnly,
			req.username, req.firstName, req.lastName, req.isPremium, req.languageCode, req.messageTime,
			out.sinks(),
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

	to := targetFor(message, businessConnectionID)

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

	// Media never waits on the text debounce window. Taking the batch over
	// does not discard the buffered text: those messages have joined chat
	// memory, so the turn this media triggers answers them too. When the
	// media path ends without a model turn, it answers the batch on its own
	// (answerPending).
	if message.MediaGroupID != "" && len(message.Photo) > 0 {
		b.bufferAlbumItem(ctx, message, to, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime, b.takeIntake(chatID))
		return
	}
	if len(message.Photo) > 0 {
		pending := b.takeIntake(chatID)
		if !b.checkRateLimits(userID) {
			b.sendRateLimitExceededMessage(ctx, to)
			b.answerPending(pending)
			return
		}
		b.handlePhotoMessage(ctx, []*models.Message{message}, pending,
			to, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime)
		return
	}

	userMsg, err := b.screenIncomingMessage(message)
	if err != nil {
		ErrorLogger.Printf("Error storing user message: %v", err)
		return
	}

	if command := handledCommand(message); command != "" {
		b.runCommand(ctx, to, command, message.Text, userID, username)
		return
	}

	if !b.checkRateLimits(userID) {
		b.setAside(&userMsg)
		b.sendRateLimitExceededMessage(ctx, to)
		return
	}

	if message.Voice != nil {
		b.handleVoiceMessage(ctx, message, userMsg, b.takeIntake(chatID), to, userID, username, firstName, lastName, isPremium, languageCode, messageTime)
		return
	}

	if message.Sticker != nil {
		b.cancelIntake(chatID)
		b.runStickerTurn(ctx, to, userMsg, message)
		return
	}

	if text == "" {
		// Stored aside at intake (unreadMedia).
		InfoLogger.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
		return
	}

	// The text passed the command and rate-limit checks: it joins the
	// conversation.
	b.joinConversation(userMsg)
	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: userID, isEmojiOnly: isOnlyEmojis(text),
		username: username, firstName: firstName, lastName: lastName,
		isPremium: isPremium, languageCode: languageCode, messageTime: messageTime,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID,
	}

	// Plain text is the only thing that debounces: it is what users fragment
	// across several sends, and it is the only kind whose meaning survives being
	// read as one turn.
	if b.config.DebounceWindow() > 0 {
		b.bufferIntake(req)
		return
	}

	b.respondToChat(req)
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
	var msgSeq uint64
	answered := false
	b.chatMemoriesMu.Lock()
	if mem, exists := b.chatMemories[chatID]; exists {
		for i := len(mem.Messages) - 1; i >= 0; i-- {
			m := &mem.Messages[i]
			if m.IsUser && m.TelegramMessageID == edited.ID {
				m.Text = text
				msgID, msgSeq = m.ID, m.Seq
				answered = m.Seq <= mem.answeredThrough.Load()
				break
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

	to := targetFor(edited, edited.BusinessConnectionID)
	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: edited.From.ID,
		username: edited.From.Username, firstName: edited.From.FirstName, lastName: edited.From.LastName,
		languageCode: edited.From.LanguageCode, isPremium: edited.From.IsPremium, messageTime: edited.Date,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID, isEmojiOnly: isEmojiOnly,
	}
	if b.restartForEdit(req, msgSeq) {
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

// handledCommands are the commands the bot answers itself. Any other /command
// is ordinary text for the model.
var handledCommands = map[string]bool{
	"/stats": true, "/whoami": true, "/clear": true, "/clear_hard": true, "/set_model": true,
}

// handledCommand is the first command in message the bot handles itself, or
// "" when there is none.
func handledCommand(message *models.Message) string {
	for _, entity := range message.Entities {
		end := entity.Offset + entity.Length
		if entity.Type != "bot_command" || entity.Offset < 0 || entity.Length < 0 || end > len(message.Text) {
			continue
		}
		if command := strings.TrimSpace(message.Text[entity.Offset:end]); handledCommands[command] {
			return command
		}
	}
	return ""
}

// unreadMedia reports whether message is media the bot does not read yet,
// which never joins the conversation. Photos never reach intake as one.
func unreadMedia(message *models.Message) bool {
	return message.Text == "" && message.Sticker == nil && message.Voice == nil
}

// runCommand answers a handled command. The command and every reply here are
// aside: they never join the conversation.
func (b *Bot) runCommand(ctx context.Context, to replyTarget, command, text string, userID int64, username string) {
	reply := func(notice string) {
		if err := b.sendResponse(ctx, to, notice); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
	}
	switch command {
	case "/stats":
		parts := strings.Fields(text)
		switch {
		case len(parts) <= 1:
			b.sendStats(ctx, to, userID, 0)
		case parts[1] == "user":
			targetUserID := userID
			if len(parts) >= 3 {
				var parseErr error
				if targetUserID, parseErr = strconv.ParseInt(parts[2], 10, 64); parseErr != nil {
					InfoLogger.Printf("User %d provided invalid user ID format: %s", userID, parts[2])
					reply("Invalid user ID format. Usage: /stats user [user_id]")
					return
				}
			}
			b.sendStats(ctx, to, userID, targetUserID)
		default:
			reply("Invalid command format. Usage: /stats or /stats user [user_id]")
		}
	case "/whoami":
		b.sendWhoAmI(ctx, to, userID, username)
	case "/clear", "/clear_hard":
		targetUserID, targetChatID, errReply := parseClearArgs(command, strings.Fields(text))
		if errReply != "" {
			InfoLogger.Printf("User %d sent invalid arguments to %s: %s", userID, command, errReply)
			reply(errReply)
			return
		}
		b.clearChatHistory(ctx, to, userID, targetUserID, targetChatID, command == "/clear_hard")
	case "/set_model":
		b.setModel(text, userID, reply)
	}
}

// setModel handles /set_model, answering through reply.
func (b *Bot) setModel(text string, userID int64, reply func(string)) {
	if !b.hasScope(userID, ScopeModelSet) {
		reply("Permission denied. Only admins and owners can change the model.")
		return
	}
	parts := strings.Fields(text)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		reply("Usage: /set_model <model-id>")
		return
	}
	newModel := strings.TrimSpace(parts[1])
	if err := b.config.PersistModel(newModel); err != nil {
		ErrorLogger.Printf("Failed to persist model change: %v", err)
		reply(fmt.Sprintf("Model updated in memory to `%s`, but failed to save to config file: %v", newModel, err))
		return
	}
	InfoLogger.Printf("Model changed to %s by user %d", newModel, userID)
	reply(fmt.Sprintf("✅ Model updated to `%s` and saved to config.", newModel))
}

func (b *Bot) sendRateLimitExceededMessage(ctx context.Context, to replyTarget) {
	if err := b.sendResponse(ctx, to, "Rate limit exceeded. Please try again later."); err != nil {
		ErrorLogger.Printf("Error sending rate limit exceeded message: %v", err)
	}
}

// runStickerTurn answers a sticker as its own committed turn, after any turn
// already running on the chat. The sticker joins the conversation inside
// that turn, so no other turn can read or cover it.
func (b *Bot) runStickerTurn(ctx context.Context, to replyTarget, userMsg Message, message *models.Message) {
	turn, turnCtx := b.beginTurn(ctx, to.chatID)
	defer b.endTurn(turn)
	b.joinConversation(userMsg)
	contextMessages := b.snapshotForTurn(turn, b.getOrCreateChatMemory(to.chatID))
	b.handleStickerMessage(turnCtx, turn, to, userMsg, message, contextMessages)
}

func (b *Bot) handleStickerMessage(ctx context.Context, turn *chatTurn, to replyTarget, userMessage Message, message *models.Message, contextMessages []anthropic.BetaMessageParam) {

	response, err := b.generateStickerResponse(ctx, to, userMessage, contextMessages)
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

	if err := b.sendTurnReply(ctx, turn, to, response); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
		return
	}
}

func (b *Bot) generateStickerResponse(ctx context.Context, to replyTarget, message Message, contextMessages []anthropic.BetaMessageParam) (string, error) {
	stopTyping := b.startChatAction(ctx, to, models.ChatActionTyping)
	defer stopTyping()

	if message.StickerFileID != "" {
		messageTime := int(message.Timestamp.Unix())
		response, err := b.getAnthropicResponse(ctx, message.ChatID, contextMessages, true, message.Username, "", "", false, "", messageTime, replySinks{})
		if err != nil {
			return "", err
		}
		return response, nil
	}

	return "Hmm, that's interesting!", nil
}

// parseClearArgs reads the optional [user_id] [chat_id] arguments shared by /clear
// and /clear_hard. On a malformed argument it returns the usage reply for the
// caller to send, naming command so each variant shows its own usage line.
func parseClearArgs(command string, parts []string) (targetUserID, targetChatID int64, errReply string) {
	var err error
	if len(parts) > 1 {
		if targetUserID, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
			return 0, 0, fmt.Sprintf("Invalid user ID format. Usage: %s [user_id] [chat_id]", command)
		}
	}
	if len(parts) > 2 {
		if targetChatID, err = strconv.ParseInt(parts[2], 10, 64); err != nil {
			return 0, 0, fmt.Sprintf("Invalid chat ID format. Usage: %s [user_id] [chat_id]", command)
		}
	}
	return targetUserID, targetChatID, ""
}

func (b *Bot) clearChatHistory(ctx context.Context, to replyTarget, currentUserID int64, targetUserID int64, targetChatID int64, hardDelete bool) {
	chatID := to.chatID
	if targetUserID != 0 && targetUserID != currentUserID {
		requiredScope := ScopeHistoryClearAny
		if hardDelete {
			requiredScope = ScopeHistoryClearHardAny
		}
		if !b.hasScope(currentUserID, requiredScope) {
			InfoLogger.Printf("User %d attempted to clear history for user %d without permission", currentUserID, targetUserID)
			if err := b.sendResponse(ctx, to, "Permission denied. Only admins and owners can clear other users' histories."); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}

		var targetUser User
		err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error
		if err != nil {
			ErrorLogger.Printf("Error finding target user %d: %v", targetUserID, err)
			if err := b.sendResponse(ctx, to, fmt.Sprintf("User with ID %d not found.", targetUserID)); err != nil {
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
		if err := b.sendResponse(ctx, to, "Sorry, I couldn't clear the chat history."); err != nil {
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

	if err := b.sendResponse(ctx, to, confirmationMessage); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
	}
}

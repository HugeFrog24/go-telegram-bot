package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/sync/errgroup"
)

// handleVoiceMessage answers a voice note as its own committed turn.
func (b *Bot) handleVoiceMessage(ctx context.Context, message *models.Message, userMsg Message, to replyTarget, userID int64, username, firstName, lastName string, isPremium bool, languageCode string, messageTime int) {
	if b.config.ElevenLabsAPIKey == "" {
		b.refuseVoice(ctx, to, "I don't understand voice messages.")
		return
	}

	if !b.hasScope(userID, ScopeTTSUse) {
		b.refuseVoice(ctx, to, "You don't have permission to use voice features.")
		return
	}

	// The note is transcribed before it asks for the chat (prepare first), so
	// text sent meanwhile is answered first, by a turn of its own.
	transcript, err := b.withTyping(ctx, to, func() (string, error) { return b.transcribeVoice(ctx, message.Voice.FileID) })
	if err == nil && strings.TrimSpace(transcript) == "" {
		err = errors.New("empty transcript")
	}
	if err != nil {
		ErrorLogger.Printf("Error transcribing voice message from user %d: %v", userID, err)
		b.refuseVoice(ctx, to, "Sorry, I couldn't understand your voice message.")
		return
	}
	userMsg.Text = transcript
	if err := b.db.Model(&Message{}).Where("id = ?", userMsg.ID).Update("text", transcript).Error; err != nil {
		ErrorLogger.Printf("Error updating voice transcript in DB: %v", err)
	}

	// Voice is a committed media turn: it waits for the chat and is never
	// restarted, so a text sent right after a voice note is answered afterwards.
	deliverCtx := ctx // for the status message: ending the turn does not cancel it
	media, ok := b.beginMediaTurn(ctx, to, userID, userMsg.ID)
	if !ok {
		return // its update's context ended while it waited
	}
	turn := media.turn
	defer b.endTurn(turn)
	ctx = media.ctx

	stopTyping := b.startChatAction(ctx, to, models.ChatActionTyping)
	defer stopTyping()

	// The note joins the conversation only now, as its transcript: until then
	// no other turn could read its placeholder, cover it, or answer it.
	joinedNote, outcome := b.joinMedia(userMsg, &media)
	if outcome != joinOK {
		b.noticeJoinFailed(deliverCtx, to, outcome) // or /clear removed the note first
		return
	}

	// Notes show while the model works. Once it is done, notes end: one still
	// waiting is dropped, so none first appears during speech synthesis. The
	// deferred close removes a status message already shown once the audio,
	// or its text fallback, is out.
	var sinks replySinks
	freezeStatus, closeStatus := noStatus, noStatus
	if status := b.startStatusLine(media.ctx, deliverCtx, to); status != nil {
		sinks.onNote = status.note
		freezeStatus, closeStatus = status.freeze, status.close
	}
	defer closeStatus()

	contextMessages, unanswered := b.snapshotForMedia(turn, b.getOrCreateChatMemory(to.conv()), joinedNote.Seq)
	if !unanswered {
		return // its sender's /clear took the note out of memory
	}
	response, err := b.getAnthropicResponse(ctx, to.conv(), contextMessages, false, username, firstName, lastName, isPremium, languageCode, messageTime, sinks)
	freezeStatus()
	if b.clearedTurn(turn) {
		return
	}
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
	if b.clearedTurn(turn) {
		return
	}
	if err != nil {
		ErrorLogger.Printf("Error generating speech, falling back to text: %v", err)
		if err := b.sendTurnReply(ctx, turn, to, response); err != nil {
			ErrorLogger.Printf("Error sending text fallback: %v", err)
		}
		return
	}

	if _, err := b.recordTurnReply(turn, to.conv(), response); errors.Is(err, errStaleReply) {
		return
	} else if err != nil {
		ErrorLogger.Printf("Error storing assistant voice response: %v", err)
	}

	if b.connectionStopped(to.businessConnectionID) {
		ErrorLogger.Printf("[%s] not sending audio to chat %d: business connection %s %v", b.config.ID, to.chatID, to.businessConnectionID, errConnectionStopped)
		return
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

// refuseVoice tells the user why a voice note will not be answered. Its row
// stays aside: it never joins the conversation.
func (b *Bot) refuseVoice(ctx context.Context, to replyTarget, notice string) {
	if err := b.sendResponse(ctx, to, notice); err != nil {
		ErrorLogger.Printf("Error sending voice notice: %v", err)
	}
}

// uploadPhotoFromItem downloads the photo on ctx, then uploads it on a
// context of its own: once started, an upload is never cut off by ctx (a
// sibling's failure cancels it), as the file could exist on the Files API
// with an id that never comes back. It finishes, or times out.
func (b *Bot) uploadPhotoFromItem(ctx context.Context, item *models.Message, row Message) (string, error) {
	photo := largestPhotoSize(item.Photo)
	data, err := b.downloadTelegramFile(ctx, photo.FileID)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", photo.FileID, err)
	}
	if !b.photoRowLive(row) {
		return "", errPhotoCleared // a clear deleted it during the download: nothing is uploaded
	}
	uploadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), photoUploadTimeout)
	defer cancel()
	filename := formatUploadFilename(b.botID, row.ChatID, item.ID, "jpg")
	return b.uploadImageToAnthropic(uploadCtx, data, filename, "image/jpeg")
}

// photoUploadTimeout bounds one photo's upload, which nothing else cancels.
const photoUploadTimeout = 2 * time.Minute

// errPhotoCleared is a photo whose row a /clear deleted before its upload.
var errPhotoCleared = errors.New("the photo's message was cleared")

// handlePhotoMessage answers a photo or an album as its own committed turn.
// row is the message stored aside on its arrival (storePhotoArrival); the
// uploads are recorded on it, and a clear that deleted it meanwhile refuses
// the photo.
func (b *Bot) handlePhotoMessage(
	ctx context.Context,
	items []*models.Message,
	row Message,
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
	var finalUploaded []string
	caption, err := b.withTyping(ctx, to, func() (caption string, err error) {
		finalUploaded, caption, err = b.uploadPhotoItems(ctx, items, row, to)
		return caption, err
	})
	if err != nil || len(finalUploaded) == 0 {
		return // its row stays aside
	}
	row.ImageFileIDs, row.Text, row.TelegramMessageID = finalUploaded, caption, items[0].ID
	if recorded, err := b.recordPhotoUploads(row); err != nil || !recorded {
		b.compensatingDelete(ctx, finalUploaded)
		if err != nil {
			ErrorLogger.Printf("[%s] recording the uploads of photo message %d failed: %v", b.config.ID, row.ID, err)
			b.sendNotice(ctx, to, "Sorry, I had trouble saving your message.")
		} else {
			InfoLogger.Printf("[%s] photo message %d in chat %d dropped: a clear deleted it during its upload",
				b.config.ID, row.ID, to.chatID)
		}
		return
	}

	req := turnRequest{
		ctx: ctx, chatID: to.chatID, userID: userID,
		username: username, firstName: firstName, lastName: lastName,
		languageCode: languageCode, isPremium: isPremium, messageTime: messageTime,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID, forum: to.forum,
	}
	// Photos are committed media turns: they wait for the chat and are never
	// restarted.
	media, ok := b.beginMediaTurn(ctx, to, userID, row.ID)
	if !ok {
		b.compensatingDelete(ctx, finalUploaded) // its update's context ended while it waited
		return
	}
	turn := media.turn

	b.runTurn(turn, media.ctx, req, func(ctx context.Context, out replyOutput) (string, error) {
		joinedPhoto, outcome := b.joinMedia(row, &media)
		if outcome != joinOK {
			b.compensatingDelete(ctx, finalUploaded) // its row stays aside; its uploads go
			b.noticeJoinFailed(req.ctx, to, outcome)
			return "", errTurnSilent
		}

		// The memory the photo just joined, which /clear may have replaced
		// since this turn began.
		contextMessages, unanswered := b.snapshotForMedia(turn, b.getOrCreateChatMemory(to.conv()), joinedPhoto.Seq)
		if !unanswered {
			b.compensatingDelete(ctx, finalUploaded) // its sender's /clear took the photo out of memory
			return "", errTurnSilent
		}
		response, err := b.getAnthropicResponse(
			ctx, to.conv(), contextMessages, false,
			username, firstName, lastName, isPremium, languageCode, messageTime,
			out.sinks(),
		)
		if b.clearedTurn(turn) {
			b.compensatingDelete(ctx, finalUploaded) // a clear deleted the photo's row: its uploads go too
		}
		return response, err
	})
}

// uploadPhotoItems uploads every photo in items to the Files API in parallel
// and returns their file ids with the album caption. On any failure it deletes
// the uploads that did succeed, tells the user, and returns errTurnSilent.
// A failed download cancels the downloads still running, never an upload
// already started (uploadPhotoFromItem): every upload that started has come
// back, with its id or not, before the deletes run.
//
// A photo a /clear deleted is not uploaded (photoRowLive, checked after each
// download); it fails nothing else: the album's other uploads finish, every
// one that came back is deleted, and nothing is said. An upload already in
// flight when the clear lands is not cancelled by it either: no clear reaches
// it (that would take a registry of each chat's uploads), so it finishes, and
// the compensation after it deletes it (here, or recordPhotoUploads).
func (b *Bot) uploadPhotoItems(ctx context.Context, items []*models.Message, row Message, to replyTarget) ([]string, string, error) {
	uploaded := make([]string, len(items))
	caption := ""
	var cleared atomic.Bool
	g, gctx := errgroup.WithContext(ctx)
	for i, item := range items {
		if item.Caption != "" {
			caption = item.Caption
		}
		if len(item.Photo) == 0 {
			continue
		}
		i, item := i, item
		g.Go(func() (err error) {
			defer func() {
				if r := recover(); r != nil { // the panic boundary, with the upload failed
					b.logPanic("photo upload", r)
					err = errUploadPanicked
				}
			}()
			fileID, err := b.uploadPhotoFromItem(gctx, item, row)
			if errors.Is(err, errPhotoCleared) {
				cleared.Store(true) // not a failure: the album's other uploads go on
				return nil
			}
			if err != nil {
				return err
			}
			uploaded[i] = fileID
			return nil
		})
	}
	if err := g.Wait(); err != nil || cleared.Load() {
		var successful []string
		for _, fid := range uploaded {
			if fid != "" {
				successful = append(successful, fid)
			}
		}
		b.compensatingDelete(ctx, successful)
		// A clear can land while the album downloads and still not be what
		// failed it first (a download error came back before any item saw
		// the clear): the album is cleared all the same, and nothing is said.
		if cleared.Load() || !b.photoRowLive(row) {
			InfoLogger.Printf("[%s] photo message %d in chat %d not uploaded: a clear deleted it", b.config.ID, row.ID, to.chatID)
			return nil, "", errTurnSilent
		}
		ErrorLogger.Printf("[%s] photo upload failed: %v", b.config.ID, err)
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
// streams the reply back. The immediate path goes through here, and the
// debounced flush through the same step (startBatch), so a coalesced turn is
// byte-for-byte the same request as a single-message one: the messages joined
// memory before they were buffered, and the model simply sees more of them.
//
// If a turn is already running for the chat, beginTextTurnLocked decides
// whether this flush is dropped, restarts it, or runs after it; see turn.go.
func (b *Bot) respondToChat(req turnRequest) {
	wantSeq, memoryGen := b.flushWant(req.conv())
	req.memoryGen = memoryGen
	chatMemory := b.getOrCreateChatMemory(req.conv())
	turn, turnCtx, ok := func() (*chatTurn, context.Context, bool) {
		b.turnsMu.Lock()
		defer b.turnsMu.Unlock()
		return b.beginFlushLocked(req, wantSeq, chatMemory)
	}()
	if ok {
		b.runTextTurn(turn, turnCtx, req)
	}
}

// beginFlushLocked registers or collects the text turn for req, which wants
// wantSeq answered (beginTextTurnLocked), and puts the message that produced
// req on record (landText) in the same step. A want of 0 means nothing of the
// user's is in memory, for example because /clear landed after the flush was
// collected: there is nothing to answer. The caller holds turnsMu.
func (b *Bot) beginFlushLocked(req turnRequest, wantSeq uint64, chatMemory *ChatMemory) (*chatTurn, context.Context, bool) {
	defer b.landText(req.conv(), req.joinedSeq)
	if wantSeq == 0 {
		InfoLogger.Printf("[%s] turn: chat %v has no user message to answer; skipping", b.config.ID, req.conv())
		return nil, nil, false
	}
	return b.beginTextTurnLocked(req, wantSeq, chatMemory)
}

// runTextTurn runs a registered text turn: it reads memory and asks the model.
// A flush whose want was read from a memory /clear has since replaced ends
// silently once its snapshot shows the change: the history it was for is gone.
func (b *Bot) runTextTurn(turn *chatTurn, turnCtx context.Context, req turnRequest) {
	b.runTurn(turn, turnCtx, req, func(ctx context.Context, out replyOutput) (string, error) {
		contextMessages, unanswered := b.snapshotForTurn(turn, b.getOrCreateChatMemory(req.conv()))
		if _, _, memoryGen := b.contextEnd(turn); req.memoryGen != 0 && memoryGen != req.memoryGen {
			InfoLogger.Printf("[%s] turn %d for chat %d dropped: its history was cleared", b.config.ID, turn.seq, req.chatID)
			return "", errTurnSilent
		}
		if !unanswered {
			InfoLogger.Printf("[%s] turn %d for chat %d dropped: nothing is left to answer", b.config.ID, turn.seq, req.chatID)
			return "", errTurnSilent
		}
		return b.getAnthropicResponse(
			ctx, req.conv(), contextMessages, req.isEmojiOnly,
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
	defer b.recoverPanic(updateKind(update))
	if stopped := update.StoppedMessageGeneration; stopped != nil {
		// Drafts stream only in a private chat with the bot: its conversation
		// is the chat alone.
		b.handleStopGeneration(convKey{chatID: stopped.Chat.ID}, stopped.DraftID)
		return
	}
	// An update holds at most one of these (Bot API, Update): a
	// business_connection update stands alone.
	if conn := update.BusinessConnection; conn != nil {
		b.handleBusinessConnection(conn)
		return
	}
	if deleted := update.DeletedBusinessMessages; deleted != nil {
		// Applied whether or not the bot still serves the connection: it only
		// deletes, and only rows of that connection's own conversation, which
		// exist only if the bot served it once (a customer's deletion after a
		// disconnect still takes their messages).
		b.handleDeletedBusinessMessages(ctx, deleted)
		return
	}
	for _, edited := range []*models.Message{update.EditedMessage, update.EditedBusinessMessage} {
		if edited == nil {
			continue
		}
		// The bot's own Business messages (its status line's edits among
		// them) are no one's to correct.
		ownEcho := edited.BusinessConnectionID != "" && b.sentByThisBot(edited)
		if _, served := b.servedOrDirect(ctx, edited.BusinessConnectionID); served && !ownEcho {
			b.handleEditedMessage(ctx, edited)
		}
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

	// A message through a Business connection is the business's only if the
	// bot serves that connection (B8): otherwise nothing of it is stored,
	// not even its sender.
	businessConnectionID := message.BusinessConnectionID
	conn, served := b.servedOrDirect(ctx, businessConnectionID)
	if !served {
		return
	}

	to := targetFor(message, businessConnectionID)

	if message.From == nil {
		return
	}

	chatID := message.Chat.ID
	from := senderOf(message)
	userID := from.id
	username := from.username
	firstName := from.firstName
	lastName := from.lastName
	languageCode := from.languageCode
	isPremium := from.isPremium
	messageTime := message.Date
	text := message.Text

	if businessConnectionID != "" && b.sentByThisBot(message) {
		// One of this bot's own replies, sent on behalf of the business
		// account: it was recorded when it was filed.
		return
	}

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

	// What the business account itself sends the customer (by hand, or
	// through another bot) is the business side of the conversation:
	// context for later replies, never answered (B9).
	if businessConnectionID != "" && (userID == conn.userID || message.SenderBusinessBot != nil) {
		b.recordOwnerMessage(message, to, from)
		return
	}

	// Media never waits on the text debounce window. Once ready, it runs any
	// text waiting there that it may not answer as a turn of its own first,
	// and takes over a batch of its own sender's (beginMediaTurn).
	if message.MediaGroupID != "" && len(message.Photo) > 0 {
		b.bufferAlbumItem(ctx, message, to, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime)
		return
	}
	if len(message.Photo) > 0 {
		row, err := b.storePhotoArrival(message, to)
		if err != nil {
			ErrorLogger.Printf("[%s] store photo message failed: %v", b.config.ID, err)
			b.sendNotice(ctx, to, "Sorry, I had trouble saving your message.")
			return
		}
		if !b.checkRateLimits(userID) {
			b.sendRateLimitExceededMessage(ctx, to)
			return
		}
		b.handlePhotoMessage(ctx, []*models.Message{message}, row,
			to, userID, username, firstName, lastName,
			isPremium, languageCode, messageTime)
		return
	}

	userMsg, err := b.screenIncomingMessage(message, to)
	if err != nil {
		ErrorLogger.Printf("Error storing user message: %v", err)
		return
	}

	command, elsewhere := handledCommand(message, b.username)
	if elsewhere {
		InfoLogger.Printf("[%s] command for another bot in chat %d ignored", b.config.ID, chatID)
		return
	}
	if command != "" {
		if b.commandAllowed(ctx, to, user) {
			b.runCommand(ctx, to, command, message.Text, from)
		}
		return
	}

	if !b.checkRateLimits(userID) {
		b.sendRateLimitExceededMessage(ctx, to)
		return
	}

	if message.Voice != nil {
		b.handleVoiceMessage(ctx, message, userMsg, to, userID, username, firstName, lastName, isPremium, languageCode, messageTime)
		return
	}

	if message.Sticker != nil {
		b.runStickerTurn(ctx, to, userMsg, message)
		return
	}

	if text == "" {
		// Media the bot does not read yet never joins: its row stays aside.
		InfoLogger.Printf("Received a non-text message from user %d in chat %d", userID, chatID)
		return
	}

	// The text passed the command and rate-limit checks: it joins the
	// conversation, unless /clear removed it first. Until its batch or turn
	// is on record, media turns wait for it (joinText).
	held, outcome := b.joinText(userMsg)
	if outcome != joinOK {
		b.noticeJoinFailed(ctx, to, outcome)
		return
	}
	defer b.landText(to.conv(), held.Seq) // given up, if nothing put it on record
	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: userID, isEmojiOnly: isOnlyEmojis(text),
		username: username, firstName: firstName, lastName: lastName,
		isPremium: isPremium, languageCode: languageCode, messageTime: messageTime,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID, forum: to.forum,
		joinedSeq: held.Seq, firstSeq: held.Seq,
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

// servedOrDirect is businessServed for an update through Business
// connection id, and serves an update with no connection.
func (b *Bot) servedOrDirect(ctx context.Context, id string) (businessConn, bool) {
	if id == "" {
		return businessConn{}, true
	}
	return b.businessServed(ctx, id)
}

// handleEditedMessage applies a Telegram edit to the stored copy of a user
// message: the one in the edit's own conversation (its chat, Business
// connection and topic), with its Telegram message id, from its sender. A
// Business chat shares its chat ID with the customer's private chat with the
// bot, and with their chats with other businesses, so an id alone could name
// a message in another conversation. Edits arrive as edited_message and as
// edited_business_message, each carrying its connection. Mobile clients
// deliver autocorrect fixes as edits within a few seconds of the send, so the
// model should see the corrected text. What happens next depends on where
// the message is:
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
	from := senderOf(edited)
	to := targetFor(edited, edited.BusinessConnectionID)
	k := to.conv()

	// Memory is the gate: only a message still in memory can influence a turn,
	// and the check is cheap. It also keeps edits from chats this bot never
	// stored off the database; a business connection delivers every edit on
	// the whole account.
	msgID, msgSeq, memoryGen, answered := b.editInMemory(k, edited.ID, from.id, text)
	if msgID == 0 {
		return
	}

	scope, args := b.convScope(k)
	if err := b.db.Model(&Message{}).Where(scope, args...).Where("id = ?", msgID).Update("text", text).Error; err != nil {
		ErrorLogger.Printf("[%s] applying edit to message %d in chat %v: %v", b.config.ID, edited.ID, k, err)
	}
	if answered || !b.editWouldAct(k, msgSeq) {
		return
	}
	// An edit that would re-arm a quiet window or restart a turn passes its
	// sender's rate limit first (B6): an edit loop costs what the messages
	// would. Over the limit, only the record is corrected.
	if !b.checkRateLimits(from.id) {
		InfoLogger.Printf("[%s] edit to message %d in chat %v only corrects the record: its sender is over the rate limit",
			b.config.ID, edited.ID, k)
		return
	}

	isEmojiOnly := isOnlyEmojis(text)
	if b.touchIntake(k, isEmojiOnly) {
		InfoLogger.Printf("[%s] edit to message %d re-armed the quiet window for chat %v", b.config.ID, edited.ID, k)
		return
	}

	req := turnRequest{
		ctx: ctx, chatID: chatID, userID: from.id,
		username: from.username, firstName: from.firstName, lastName: from.lastName,
		languageCode: from.languageCode, isPremium: from.isPremium, messageTime: edited.Date,
		businessConnectionID: to.businessConnectionID, threadID: to.threadID, forum: to.forum, isEmojiOnly: isEmojiOnly,
		memoryGen: memoryGen, joinedSeq: msgSeq, firstSeq: msgSeq,
	}
	if b.restartForEdit(req, msgSeq) {
		InfoLogger.Printf("[%s] edit to message %d restarted the running turn for chat %v", b.config.ID, edited.ID, k)
	}
}

// editInMemory applies an edit to the memory copy of the user message with
// Telegram id telegramID that senderID sent, in conversation k, under the
// memory lock, and reports that message: its row id (0 when memory does not
// hold it), sequence, memory generation, and whether a reply already covers
// it.
func (b *Bot) editInMemory(k convKey, telegramID int, senderID int64, text string) (msgID uint, msgSeq, memoryGen uint64, answered bool) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	mem, exists := b.chatMemories[k]
	if !exists {
		return 0, 0, 0, false
	}
	for i := len(mem.Messages) - 1; i >= 0; i-- {
		m := &mem.Messages[i]
		if (!m.IsUser && !ownerSide(*m)) || m.TelegramMessageID != telegramID || m.UserID != senderID {
			continue
		}
		m.Text = text
		// A business owner's message answers nothing and waits for nothing:
		// its edit only corrects the record.
		return m.ID, m.Seq, mem.generation, ownerSide(*m) || m.Seq <= mem.answeredThrough.Load()
	}
	return 0, 0, 0, false
}

// handleStopGeneration routes Telegram's stopped_message_generation update,
// sent when the user taps Stop under a streaming draft.
func (b *Bot) handleStopGeneration(k convKey, draftID int) {
	if b.stopTurn(k, draftID) {
		InfoLogger.Printf("[%s] user stopped generation in chat %v (draft %d)", b.config.ID, k, draftID)
		return
	}
	InfoLogger.Printf("[%s] stop for chat %v ignored: draft %d is not the running turn", b.config.ID, k, draftID)
}

// handledCommands are the commands the bot answers itself. Any other /command
// is ordinary text for the model.
var handledCommands = map[string]bool{
	"/stats": true, "/whoami": true, "/clear": true, "/clear_hard": true, "/set_model": true,
}

// handledCommand is the command the bot handles itself that message starts
// with, or "" when there is none. A command anywhere else ("what does /stats
// do?") is part of what the user is saying, and goes to the model as text.
//
// A command addressed to a bot by name counts only when the name is this
// bot's, botUsername, compared without regard to case: /stats@thisbot is
// /stats. elsewhere
// reports a message that starts with a command addressed to another bot, or
// to any bot while this one's name is unknown: in a group with several bots
// it is meant for that one, so it is neither run nor answered as text.
func handledCommand(message *models.Message, botUsername string) (command string, elsewhere bool) {
	var byteAt []int
	for _, entity := range message.Entities {
		if entity.Type != "bot_command" || entity.Offset != 0 {
			continue
		}
		if byteAt == nil {
			byteAt = utf16ByteOffsets(message.Text)
		}
		name, addressee, addressed := strings.Cut(entityText(message.Text, byteAt, entity.Offset, entity.Length), "@")
		if addressed && (botUsername == "" || !strings.EqualFold(addressee, botUsername)) {
			return "", true
		}
		if handledCommands[name] {
			return name, false
		}
	}
	return "", false
}

// utf16ByteOffsets maps each UTF-16 code unit position in text, and its end,
// to a byte offset: Telegram measures entities in UTF-16 code units. A
// position inside a surrogate pair maps to -1.
func utf16ByteOffsets(text string) []int {
	byteAt := make([]int, 0, len(text)+1)
	for i, r := range text {
		byteAt = append(byteAt, i)
		if utf16.RuneLen(r) == 2 {
			byteAt = append(byteAt, -1)
		}
	}
	return append(byteAt, len(text))
}

// entityText is the trimmed text of the entity at offset and length (UTF-16
// units), or "" when the range is out of bounds or splits a character. The
// check cannot overflow, however large the values Telegram, or anyone
// posing as it, sends.
func entityText(text string, byteAt []int, offset, length int) string {
	n := len(byteAt) - 1
	if offset < 0 || length <= 0 || offset > n || length > n-offset {
		return ""
	}
	start, end := byteAt[offset], byteAt[offset+length]
	if start < 0 || end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start:end])
}

// runCommand answers a handled command from from. The command and every
// reply here are aside: they never join the conversation.
func (b *Bot) runCommand(ctx context.Context, to replyTarget, command, text string, from profile) {
	userID, username := from.id, from.username
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
		b.clearChatHistory(ctx, to, from, targetUserID, targetChatID, command == "/clear_hard")
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
	media, ok := b.beginMediaTurn(ctx, to, userMsg.UserID, userMsg.ID)
	if !ok {
		return // its update's context ended while it waited
	}
	turn := media.turn
	defer b.endTurn(turn)
	joinedSticker, outcome := b.joinMedia(userMsg, &media)
	if outcome != joinOK {
		b.noticeJoinFailed(ctx, to, outcome) // or /clear removed the sticker first
		return
	}
	contextMessages, unanswered := b.snapshotForMedia(turn, b.getOrCreateChatMemory(to.conv()), joinedSticker.Seq)
	if !unanswered {
		return // its sender's /clear took the sticker out of memory
	}
	// A sticker answers in emoji, unless it took over text of its sender's
	// that was not emoji only.
	emojiOnly := media.folded == nil || media.folded.allEmojiOnly
	b.handleStickerMessage(media.ctx, turn, to, userMsg, message, contextMessages, emojiOnly)
}

func (b *Bot) handleStickerMessage(ctx context.Context, turn *chatTurn, to replyTarget, userMessage Message, message *models.Message, contextMessages []anthropic.BetaMessageParam, emojiOnly bool) {

	response, err := b.generateStickerResponse(ctx, to, userMessage, contextMessages, emojiOnly)
	if b.clearedTurn(turn) {
		return
	}
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

func (b *Bot) generateStickerResponse(ctx context.Context, to replyTarget, message Message, contextMessages []anthropic.BetaMessageParam, emojiOnly bool) (string, error) {
	stopTyping := b.startChatAction(ctx, to, models.ChatActionTyping)
	defer stopTyping()

	if message.StickerFileID != "" {
		messageTime := int(message.Timestamp.Unix())
		response, err := b.getAnthropicResponse(ctx, message.conv(), contextMessages, emojiOnly, message.Username, "", "", false, "", messageTime, replySinks{})
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

// anonymousClearRefused answers an anonymous admin's bare /clear in a group:
// Telegram sends every anonymous admin as the group itself, so the bot cannot
// tell whose messages are whose.
const anonymousClearRefused = "Anonymous admins can't clear only their own messages: Telegram shows them all as the group. Post as yourself to /clear, or ask a bot admin to clear the chat."

// deleteClearedRows deletes the rows of a cleared scope up to the clear's
// cutoff: soft, or with /clear_hard permanently, files included.
func (b *Bot) deleteClearedRows(ctx context.Context, hard bool, cutoff uint, query string, args ...any) error {
	if hard {
		return b.hardDeleteScope(ctx, cutoff, query, args...)
	}
	return b.db.Where(query, args...).Where("id <= ?", cutoff).Delete(&Message{}).Error
}

// clearChatHistory runs /clear or /clear_hard. Every branch checks its own
// scope (history:clear:own and history:clear:any; for /clear_hard,
// history:clear_hard:own and history:clear_hard:any). A bare clear reaches
// only the conversation it was sent in (convKey: its chat, its Business
// connection, its forum topic):
//
//   - bare (or naming the sender, and this chat or none) in a private chat
//     with the bot: the whole conversation, which is the sender's own
//     history (own);
//   - bare in a chat other people share (a group, a supergroup, a Business
//     chat): the whole conversation (any), or else the sender's own messages
//     there (own; clearOwnMessages);
//   - /clear <user_id> [chat_id]: that user's own rows, in every conversation
//     with that chat ID (every Business connection's and every topic's) or in
//     every chat, never anyone else's (any; clearUserMessages); a form that
//     covers the user's private chat with the bot (the conversation with
//     their ID and no connection) clears that one whole. A Business chat
//     with the user as the customer has the same chat ID, but only their
//     own rows go there: the business owner's messages and the bot's
//     Business replies stay.
//
// A role without the scope a branch needs is refused, and so is an anonymous
// admin's clear of their own messages in a shared chat: Telegram sends every
// anonymous admin as the group itself, so theirs cannot be told apart (B11).
func (b *Bot) clearChatHistory(ctx context.Context, to replyTarget, from profile, targetUserID int64, targetChatID int64, hardDelete bool) {
	currentUserID := from.id
	ownScope, anyScope := ScopeHistoryClearOwn, ScopeHistoryClearAny
	if hardDelete {
		ownScope, anyScope = ScopeHistoryClearHardOwn, ScopeHistoryClearHardAny
	}
	if targetUserID == 0 || (targetUserID == currentUserID && (targetChatID == 0 || targetChatID == to.chatID)) {
		switch {
		case to.shared && b.hasScope(currentUserID, anyScope):
			b.clearWholeChat(ctx, to, currentUserID, hardDelete)
		case !b.hasScope(currentUserID, ownScope):
			InfoLogger.Printf("User %d attempted to clear their history without the %s scope", currentUserID, ownScope)
			b.sendNotice(ctx, to, "Permission denied. Your role can't clear chat history.")
		case to.shared && from.anonymous:
			InfoLogger.Printf("[%s] anonymous admin's own clear in chat %v refused", b.config.ID, to.conv())
			b.sendNotice(ctx, to, anonymousClearRefused)
		case to.shared:
			b.clearOwnMessages(ctx, to, currentUserID, hardDelete)
		default:
			b.clearWholeChat(ctx, to, currentUserID, hardDelete)
		}
		return
	}

	if !b.hasScope(currentUserID, anyScope) {
		InfoLogger.Printf("User %d attempted to clear history for user %d without permission", currentUserID, targetUserID)
		b.sendNotice(ctx, to, "Permission denied. Only admins and owners can clear other users' histories.")
		return
	}
	var targetUser User
	if err := b.db.Where("telegram_id = ? AND bot_id = ?", targetUserID, b.botID).First(&targetUser).Error; err != nil {
		ErrorLogger.Printf("Error finding target user %d: %v", targetUserID, err)
		b.sendNotice(ctx, to, fmt.Sprintf("User with ID %d not found.", targetUserID))
		return
	}
	var cutoff uint
	var err error
	// The target's private chat with the bot (the conversation with their ID
	// and no Business connection) is wholly theirs: a form that covers it
	// clears it whole, the bot's replies, which echo what was deleted,
	// included (F3). A Business chat with the target as the customer has the
	// same chat ID but is shared with the business: there, as in a group,
	// only the target's own rows go (B3).
	if targetChatID == 0 || targetChatID == targetUserID {
		cutoff, err = b.clearChat(ctx, hardDelete, convKey{chatID: targetUserID})
	}
	if err == nil {
		cutoff, err = b.clearUserMessages(ctx, hardDelete, userClear{userID: targetUserID, chatID: targetChatID})
	}
	if !b.clearSucceeded(ctx, to, err) {
		return
	}
	InfoLogger.Printf("User %d cleared the messages (hard=%v) of user %d in chat %d (0: every chat) up to message %d",
		currentUserID, hardDelete, targetUserID, targetChatID, cutoff)
	confirmation := fmt.Sprintf("Chat history for user with ID %d has been cleared.", targetUserID)
	if targetUser.Username != "" {
		confirmation = fmt.Sprintf("Chat history for user @%s (ID: %d) has been cleared.", targetUser.Username, targetUserID)
	}
	b.sendNotice(ctx, to, confirmation)
}

// clearWholeChat clears the chat /clear was sent in, everyone's messages and
// the bot's: in one step the conversation is replaced and its turn cancelled
// before any row goes (beginClear), then the rows up to the cutoff go.
func (b *Bot) clearWholeChat(ctx context.Context, to replyTarget, userID int64, hardDelete bool) {
	cutoff, err := b.clearChat(ctx, hardDelete, to.conv())
	if !b.clearSucceeded(ctx, to, err) {
		return
	}
	InfoLogger.Printf("User %d cleared chat history (hard=%v) in chat %v up to message %d",
		userID, hardDelete, to.conv(), cutoff)
	b.sendNotice(ctx, to, "Your chat history has been cleared.")
}

// clearChat clears conversation k whole, everyone's messages and the bot's,
// files too for a hard clear: beginClear's one step, then the rows up to its
// cutoff.
func (b *Bot) clearChat(ctx context.Context, hardDelete bool, k convKey) (uint, error) {
	cutoff, err := b.beginClear(k)
	if err != nil {
		return 0, err
	}
	scope, args := b.convScope(k)
	return cutoff, b.deleteClearedRows(ctx, hardDelete, cutoff, scope, args...)
}

// clearOwnMessages is a bare /clear or /clear_hard in a chat other people
// share from someone with only the :own scope: their own messages in this
// chat go (clearUserMessages), and the conversation goes on for everyone else.
func (b *Bot) clearOwnMessages(ctx context.Context, to replyTarget, userID int64, hardDelete bool) {
	k := to.conv()
	cutoff, err := b.clearUserMessages(ctx, hardDelete, userClear{userID: userID, conv: &k})
	if !b.clearSucceeded(ctx, to, err) {
		return
	}
	InfoLogger.Printf("User %d cleared their own messages (hard=%v) in chat %v up to message %d",
		userID, hardDelete, k, cutoff)
	b.sendNotice(ctx, to, "Your messages in this chat were cleared.")
}

// clearUserMessages deletes the user's own rows (user_id = c.userID) in the
// conversations c covers, from the database and from memory, up to the
// cutoff beginUserClear takes. In that same step a running turn and a
// waiting batch that hold their messages are dealt with
// (stopForUserClearLocked): one answering only their messages, or their own
// media message, ends silently; one that also answers others restarts
// without them if it has shown nothing, or else finishes with its reply
// stored aside; a waiting batch of theirs alone is dropped.
func (b *Bot) clearUserMessages(ctx context.Context, hardDelete bool, c userClear) (uint, error) {
	cutoff, err := b.beginUserClear(c)
	if err != nil {
		return 0, err
	}
	query, args := c.rows(b)
	if err := b.deleteClearedRows(ctx, hardDelete, cutoff, query, args...); err != nil {
		return 0, err
	}
	return cutoff, nil
}

// clearSucceeded reports whether a clear's err is nil, and otherwise tells
// the user it failed.
func (b *Bot) clearSucceeded(ctx context.Context, to replyTarget, err error) bool {
	if err == nil {
		return true
	}
	ErrorLogger.Printf("Error clearing chat history: %v", err)
	b.sendNotice(ctx, to, "Sorry, I couldn't clear the chat history.")
	return false
}

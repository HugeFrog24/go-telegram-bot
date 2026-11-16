package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"gorm.io/gorm"
)

type Bot struct {
	tgBot           TelegramClient
	db              *gorm.DB
	anthropicClient anthropic.Client
	chatMemories    map[int64]*ChatMemory
	memorySize      int
	chatMemoriesMu  sync.RWMutex
	config          BotConfig
	userLimiters    map[int64]*userLimiter
	userLimitersMu  sync.RWMutex
	clock           Clock
	botID           uint
	albumBuffers    map[string]*pendingAlbum
	albumBuffersMu  sync.Mutex
	intakeBuffers   map[int64]*pendingIntake
	intakeBuffersMu sync.Mutex
	intakeSeq       uint64
	turns           map[int64]*chatTurn
	turnsMu         sync.Mutex
	turnSeq         uint64

	// memorySeq is the last sequence given to a message in chat memory
	// (Message.Seq). Guarded by chatMemoriesMu.
	memorySeq uint64

	// ElevenLabs endpoints: fields so tests can point them at a stand-in.
	sttURL, ttsURL string

	// Drafts: the send throttle, the keepalive and how the sender sleeps
	// (fields so tests can shorten the timings and count the sender's timers).
	draftInterval  time.Duration
	draftKeepalive time.Duration
	draftAfter     func(time.Duration) <-chan time.Time

	// Progress-note status messages: the first-send delay and edit throttle
	// (fields so tests can shorten them), the per-connection cache of
	// Business rights, and the lines a shutdown waits for.
	statusFirstDelay   time.Duration
	statusEditInterval time.Duration
	businessRights     map[string]businessRightsEntry
	businessRightsMu   sync.Mutex
	statusLines        openStatusLines

	// video is the defaulted video block; nil means video watching is off
	// (no block configured, ffmpeg/ffprobe not found as absolute paths, no
	// sandbox without video.allow_unsandboxed, or a failed self-test).
	// It is the feature's only switch: later code tests b.video (or
	// videoOn), never b.config.Video, which stays set when the tools are missing.
	video *VideoConfig
}

// videoOn reports whether the bot watches videos.
func (b *Bot) videoOn() bool {
	return b.video != nil
}

// The gate's Landlock probe and self-test; variables so tests can stand in
// for a host without Landlock or for tools that cannot pass a real self-test.
var (
	gateSandbox  = sandboxAvailable
	gateSelfTest = toolSelfTest
)

// resolveVideo returns the defaulted video block when the feature can run:
// the block is configured, both ffmpeg and ffprobe resolve to absolute paths
// on PATH (lookupTool), the tools' limits can be set, no sandbox rule reaches
// the bot's own directories, the Landlock sandbox is there (or
// video.allow_unsandboxed accepts its absence, with a loud warning), and the
// self-test passes through the production runner. Otherwise it returns nil,
// logging why when the block asked for the feature.
func resolveVideo(config *BotConfig) *VideoConfig {
	if config.Video == nil {
		return nil
	}
	if err := videoRunnable(); err != nil {
		InfoLogger.Printf("[%s] video: %v; video watching is off", config.ID, err)
		return nil
	}
	// Landlock before ABI 3 cannot deny truncation: no sandbox (Ruling 15).
	abi, probeErr := gateSandbox()
	noSandbox := sandboxUsable(abi, probeErr)
	granted := false
	// A sandbox that is not whole turns video off, unless the block's
	// video.allow_unsandboxed accepts it; the loud warning comes once the
	// self-test has reported which layers are missing.
	acceptUnsandboxed := func(why error) bool {
		if !config.Video.AllowUnsandboxed {
			InfoLogger.Printf("[%s] video: no sandbox for ffmpeg (%v); video watching is off (video.allow_unsandboxed runs ffmpeg without one)",
				config.ID, why)
			return false
		}
		unsandboxedGrants.Add(1)
		granted = true
		return true
	}
	if noSandbox != nil && !acceptUnsandboxed(noSandbox) {
		return nil
	}
	report, err := gateSelfTest(context.Background(), config.Video.FontFile, noSandbox == nil)
	if noSandbox == nil && errors.Is(err, errTaskCapUnbound) {
		// Only the check run can tell: the task cap is probed after the
		// helper drops its capabilities (Ruling 19 item 4).
		noSandbox = err
		if !acceptUnsandboxed(err) {
			return nil
		}
		report, err = gateSelfTest(context.Background(), config.Video.FontFile, false)
	}
	if err != nil {
		if granted {
			unsandboxedGrants.Add(-1) // this bot's video is off: its grant goes too
		}
		InfoLogger.Printf("[%s] video: the ffmpeg self-test failed: %v, stderr %q; video watching is off", config.ID, err, toolStderr(err))
		return nil
	}
	if noSandbox != nil {
		// From the self-test's report, so every missing layer is named, not
		// only the first the probe met (Ruling 21 item 2).
		ErrorLogger.Printf("[%s] Warning: video: ffmpeg runs WITHOUT a full sandbox (%v) because video.allow_unsandboxed is set: "+
			"a video that exploits ffmpeg could, %s", config.ID, noSandbox, unsandboxedHarm(sandboxHelper, report))
	}
	logSandbox(config.ID, noSandbox == nil, report)
	logToolVersion(config.ID)
	video := config.Video.withDefaults()
	return &video
}

// videoRunnable says why no tool can run for video at all, or returns nil.
func videoRunnable() error {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := lookupTool(tool); err != nil {
			return errors.New("ffmpeg/ffprobe not found on PATH")
		}
	}
	if err := toolLimitsUsable(); err != nil {
		return err
	}
	return sandboxSparesBot()
}

// unsandboxedHarm is what a video that exploits ffmpeg could do for want of
// each layer the self-test's report r shows missing, for the opt-out's
// warning: Landlock (Ruling 19 item 8: reading, writing and running files),
// the seccomp filter, and a task cap that binds the user, every one of them
// (Ruling 21 item 2). helper is whether the sandbox helper runs the tools
// (sandboxHelper); without it there is no layer at all. The gaps line lists
// the rest.
func unsandboxedHarm(helper bool, r sandboxReport) string {
	if !helper {
		return "with no sandbox on this OS, do anything the bot's user can: read, write and run any file " +
			"(every bot's token, the chat database, other chats' media) and use the network"
	}
	var harms []string
	switch {
	case r.ABI == 0:
		harms = append(harms, "without Landlock, read, write and run any file the bot's user can (every bot's token, the chat database, other chats' media)")
	case r.ABI < minSandboxABI:
		harms = append(harms, fmt.Sprintf("with Landlock ABI %d, below %d, empty any file the bot's user can write "+
			"(the chat database, stored videos, a writable config)", r.ABI, minSandboxABI))
	}
	if !r.Seccomp {
		harms = append(harms, "without the seccomp filter, fork processes that leave the tool's group and outlive the run, and use what else "+
			"only the filter closes, as the gaps line lists (other processes, SysV IPC, ptrace, the sockets its Landlock ABI allows)")
	}
	if !r.TaskCap {
		harms = append(harms, "without a task cap (root), start threads until the host, or a container's pids limit, runs out of them")
	}
	if len(harms) == 0 {
		return "do what the gaps line lists"
	}
	return strings.Join(harms, "; and, ")
}

// logSandbox logs the sandbox the self-test reported, in every mode that runs
// video (sandboxSummary): its layers, a partial sandbox's too, and, once,
// what they leave open (Rulings 15, 16 and 19); and when oom_score_adj
// could not be set.
func logSandbox(botID string, sandboxed bool, report sandboxReport) {
	for _, line := range sandboxSummary(sandboxHelper, sandboxed, report) {
		InfoLogger.Printf("[%s] video: %s", botID, line)
	}
	if toolMemoryCapped && !report.OOMScoreAdj {
		InfoLogger.Printf("[%s] video: oom_score_adj could not be set; under memory pressure the OOM killer may pick the bot before ffmpeg", botID)
	}
}

// logToolVersion logs the ffmpeg version line when the gate turns video on,
// and warns when the version is below minFFmpegMajor or cannot be read. It is
// never fatal (Ruling 12 item 6).
func logToolVersion(botID string) {
	line, err := toolVersion(context.Background())
	if err != nil {
		ErrorLogger.Printf("[%s] Warning: video: could not read the ffmpeg version: %v", botID, err)
		return
	}
	InfoLogger.Printf("[%s] video: %q", botID, line)
	if major, ok := ffmpegMajor(line); !ok || major < minFFmpegMajor {
		ErrorLogger.Printf("[%s] Warning: video: ffmpeg below version %d or of unknown version; it may lack security fixes for untrusted media",
			botID, minFFmpegMajor)
	}
}

func messageType(msg *models.Message) string {
	if msg.Sticker != nil {
		return "sticker"
	}
	return "text"
}

func NewBot(db *gorm.DB, config BotConfig, clock Clock, tgClient TelegramClient) (*Bot, error) {
	var botEntry BotModel
	err := db.Where("identifier = ?", config.ID).First(&botEntry).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		botEntry = BotModel{Identifier: config.ID, Name: config.ID}
		if err := db.Create(&botEntry).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	var owner User
	err = db.Where("telegram_id = ? AND bot_id = ?", config.OwnerTelegramID, botEntry.ID).First(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var ownerRole Role
		err := db.Where("name = ?", "owner").First(&ownerRole).Error
		if err != nil {
			return nil, fmt.Errorf("owner role not found: %w", err)
		}

		owner = User{
			BotID:      botEntry.ID,
			TelegramID: config.OwnerTelegramID,
			Username:   "",
			RoleID:     ownerRole.ID,
			IsOwner:    true,
		}

		if err := db.Create(&owner).Error; err != nil {
			if strings.Contains(err.Error(), "unique index") {
				return nil, fmt.Errorf("an owner already exists for this bot")
			}
			return nil, fmt.Errorf("failed to create owner user: %w", err)
		}
	} else if err != nil {
		return nil, err
	}

	// An explicitly passed credential is used as is, so an empty key would go
	// out as an empty bearer token; leave it out to use the SDK's default chain.
	var anthropicOpts []option.RequestOption
	if config.AnthropicAPIKey != "" {
		anthropicOpts = append(anthropicOpts, option.WithAPIKey(config.AnthropicAPIKey))
	}
	anthropicClient := anthropic.NewClient(anthropicOpts...)

	b := &Bot{
		db:              db,
		anthropicClient: anthropicClient,
		chatMemories:    make(map[int64]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
		clock:           clock,
		botID:           botEntry.ID,
		tgBot:           tgClient,
		albumBuffers:    make(map[string]*pendingAlbum),
		intakeBuffers:   make(map[int64]*pendingIntake),
		turns:           make(map[int64]*chatTurn),

		draftInterval:  draftUpdateInterval,
		draftKeepalive: draftKeepaliveInterval,
		draftAfter:     time.After,

		statusFirstDelay:   defaultStatusFirstDelay,
		statusEditInterval: defaultStatusEditInterval,
		businessRights:     make(map[string]businessRightsEntry),

		sttURL: elevenLabsSTTURL,
		ttsURL: elevenLabsTTSURL,

		video: resolveVideo(&config),
	}

	if tgClient == nil {
		var err error
		tgClient, err = initTelegramBot(config.TelegramToken, b)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Telegram bot: %w", err)
		}
		b.tgBot = tgClient
	}

	return b, nil
}

func (b *Bot) Start(ctx context.Context) {
	b.tgBot.Start(ctx)
}

func (b *Bot) getOrCreateUser(userID int64, username string, isOwner bool) (User, error) {
	var user User
	err := b.db.Preload("Role").Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if isOwner {
				var existingOwner User
				err := b.db.Where("bot_id = ? AND is_owner = ?", b.botID, true).First(&existingOwner).Error
				if err == nil {
					return User{}, fmt.Errorf("an owner already exists for this bot")
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return User{}, fmt.Errorf("failed to check existing owner: %w", err)
				}
			}

			var role Role
			var roleName string
			if isOwner {
				roleName = "owner"
			} else {
				roleName = "user"
			}

			err := b.db.Where("name = ?", roleName).First(&role).Error
			if err != nil {
				return User{}, fmt.Errorf("failed to get role: %w", err)
			}

			user = User{
				BotID:      b.botID,
				TelegramID: userID,
				Username:   username,
				RoleID:     role.ID,
				Role:       role,
				IsOwner:    isOwner,
			}

			if err := b.db.Create(&user).Error; err != nil {
				if strings.Contains(err.Error(), "unique index") {
					return User{}, fmt.Errorf("an owner already exists for this bot")
				}
				return User{}, fmt.Errorf("failed to create user: %w", err)
			}
		} else {
			return User{}, err
		}
	} else {
		if isOwner && !user.IsOwner {
			return User{}, fmt.Errorf("cannot change existing user to owner")
		}
	}

	return user, nil
}

func (b *Bot) getRoleByName(roleName string) (Role, error) {
	var role Role
	err := b.db.Where("name = ?", roleName).First(&role).Error
	return role, err
}

func (b *Bot) createMessage(chatID, userID int64, username, userRole, text string, isUser bool) Message {
	message := Message{
		ChatID:    chatID,
		UserRole:  userRole,
		Text:      text,
		Timestamp: time.Now(),
		IsUser:    isUser,
	}

	if isUser {
		message.UserID = userID
		message.Username = username
	} else {
		message.UserID = 0
		message.Username = "AI Assistant"
	}

	return message
}

func (b *Bot) storeMessage(message *Message) error {
	message.BotID = b.botID
	return b.db.Create(message).Error
}

func (b *Bot) getOrCreateChatMemory(chatID int64) *ChatMemory {
	b.chatMemoriesMu.RLock()
	chatMemory, exists := b.chatMemories[chatID]
	b.chatMemoriesMu.RUnlock()
	if exists {
		return chatMemory
	}

	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	if chatMemory, exists = b.chatMemories[chatID]; exists {
		return chatMemory
	}
	history := b.loadChatHistory(chatID)
	chatMemory = &ChatMemory{Messages: make([]Message, 0, len(history)), Size: b.memorySize * 2}
	for _, message := range history {
		b.insertLocked(chatMemory, message, len(chatMemory.Messages))
	}
	chatMemory.answeredThrough.Store(reloadedAnsweredThrough(chatMemory.Messages))
	b.chatMemories[chatID] = chatMemory
	return chatMemory
}

// loadChatHistory reads the chat's newest conversation messages from the
// database, oldest first, for a memory being created: at startup, or after
// /clear dropped it. Aside rows are skipped. Messages with the same timestamp
// keep the order they were stored in.
func (b *Bot) loadChatHistory(chatID int64) []Message {
	var messages []Message
	err := b.db.Where("chat_id = ? AND bot_id = ? AND (aside = ? OR aside IS NULL)", chatID, b.botID, false).
		Order("timestamp desc, id desc").
		Limit(b.memorySize * 2).
		Find(&messages).Error
	if err != nil {
		ErrorLogger.Printf("Error fetching messages from database: %v", err)
		return nil
	}
	slices.Reverse(messages)
	return messages
}

// reloadedAnsweredThrough is answeredThrough for a memory just loaded from the
// database: the last answerable user message before the last bot message. A
// model reply follows the messages its turn read, so those before the last
// reply are answered and those after it are not.
func reloadedAnsweredThrough(messages []Message) uint64 {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].IsUser {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if answerable(messages[j]) {
				return messages[j].Seq
			}
		}
		return 0
	}
	return 0
}

// answerable reports whether msg is a user message the model can be asked to
// answer: one with content to send.
func answerable(msg Message) bool {
	return msg.IsUser && len(contentBlocksForMessage(msg)) > 0
}

func (b *Bot) stripDeadFileIDFromMemory(chatID int64, deadFileID string) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	cm, exists := b.chatMemories[chatID]
	if !exists {
		return
	}
	for i := range cm.Messages {
		if len(cm.Messages[i].ImageFileIDs) == 0 {
			continue
		}
		survivors := make([]string, 0, len(cm.Messages[i].ImageFileIDs))
		for _, fid := range cm.Messages[i].ImageFileIDs {
			if fid != deadFileID {
				survivors = append(survivors, fid)
			}
		}
		cm.Messages[i].ImageFileIDs = survivors
	}
}

// joinConversation appends a stored user message to its chat's memory, once
// it joins the conversation, and returns the copy memory holds, which carries
// its sequence. Until then no turn can read it, cover it, or mark it
// answered. A memory reloaded since the message was stored may hold it
// already; that copy then takes the message's current text.
func (b *Bot) joinConversation(message Message) Message {
	chatMemory := b.getOrCreateChatMemory(message.ChatID)
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	for i := range chatMemory.Messages {
		if held := &chatMemory.Messages[i]; message.ID != 0 && held.ID == message.ID {
			held.Text = message.Text
			return *held
		}
	}
	return b.insertLocked(chatMemory, message, len(chatMemory.Messages))
}

// insertLocked puts message into memory at index at with the next sequence
// number, then trims the window from the front, and returns the copy memory
// holds. Sequences only grow, across chats and reloads, so one taken before a
// message was inserted is always below that message's. The caller holds
// chatMemoriesMu for writing.
func (b *Bot) insertLocked(chatMemory *ChatMemory, message Message, at int) Message {
	b.memorySeq++
	message.Seq = b.memorySeq
	chatMemory.Messages = slices.Insert(chatMemory.Messages, at, message)
	if excess := len(chatMemory.Messages) - chatMemory.Size; excess > 0 {
		chatMemory.Messages = chatMemory.Messages[excess:]
	}
	return message
}

// fileReply puts a turn's reply into memory right after the last message at
// or below frontier, or at the end when frontier is 0, and raises the chat's
// answeredThrough to covered. Messages that arrived while the turn ran have
// higher sequences, so they stay after the reply. It returns the database ids
// of the user messages the reply newly answers.
func (b *Bot) fileReply(chatMemory *ChatMemory, reply Message, frontier, covered uint64) []uint {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()

	before := chatMemory.answeredThrough.Load()
	var answered []uint
	for _, m := range chatMemory.Messages {
		if m.IsUser && m.ID != 0 && m.Seq > before && m.Seq <= covered {
			answered = append(answered, m.ID)
		}
	}
	b.insertLocked(chatMemory, reply, replyIndex(chatMemory, frontier))
	if covered > before {
		chatMemory.answeredThrough.Store(covered)
	}
	return answered
}

// replyPredecessor is the timestamp of the message a reply to a context that
// ended at frontier will follow, and whether later messages will follow the
// reply. It is false for a reply that goes at the end, or at the front.
func (b *Bot) replyPredecessor(chatMemory *ChatMemory, frontier uint64) (time.Time, bool) {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	at := replyIndex(chatMemory, frontier)
	if at == 0 || at == len(chatMemory.Messages) {
		return time.Time{}, false
	}
	return chatMemory.Messages[at-1].Timestamp, true
}

// replyIndex is where a reply to a context that ended at frontier goes: right
// after the last message at or below it; at the end for frontier 0; at the
// front, to be trimmed at once, when the window has moved past it.
func replyIndex(chatMemory *ChatMemory, frontier uint64) int {
	if frontier == 0 {
		return len(chatMemory.Messages)
	}
	for i := len(chatMemory.Messages) - 1; i >= 0; i-- {
		if chatMemory.Messages[i].Seq <= frontier {
			return i + 1
		}
	}
	return 0
}

// snapshotForTurn builds the model context for turn and records on it what
// that context covers, all read under one lock so they cannot disagree: the
// newest answerable user message, which lets a later flush tell whether the
// turn answers it, and the newest message of any kind, where the turn's reply
// goes (fileReply).
func (b *Bot) snapshotForTurn(turn *chatTurn, chatMemory *ChatMemory) []anthropic.BetaMessageParam {
	b.chatMemoriesMu.RLock()
	contextMessages := b.buildContextMessages(chatMemory)
	covered := newestAnswerableSeq(chatMemory)
	frontier := newestSeq(chatMemory)
	b.chatMemoriesMu.RUnlock()

	b.markSnapshot(turn, covered, frontier)
	return contextMessages
}

// newestAnswerableSeq is the highest sequence among the answerable user
// messages in memory, or 0. The caller holds chatMemoriesMu.
func newestAnswerableSeq(chatMemory *ChatMemory) uint64 {
	var newest uint64
	for _, msg := range chatMemory.Messages {
		if answerable(msg) {
			newest = max(newest, msg.Seq)
		}
	}
	return newest
}

// newestSeq is the highest sequence in memory, or 0. The caller holds
// chatMemoriesMu.
func newestSeq(chatMemory *ChatMemory) uint64 {
	var newest uint64
	for _, msg := range chatMemory.Messages {
		newest = max(newest, msg.Seq)
	}
	return newest
}

// newestAnswerableSeqInChat is what a text flush for the chat wants answered.
func (b *Bot) newestAnswerableSeqInChat(chatID int64) uint64 {
	chatMemory := b.getOrCreateChatMemory(chatID)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	return newestAnswerableSeq(chatMemory)
}

// buildContextMessages converts chat memory into API messages. The caller
// holds chatMemoriesMu.
func (b *Bot) buildContextMessages(chatMemory *ChatMemory) []anthropic.BetaMessageParam {
	InfoLogger.Printf("Chat memory contains %d messages", len(chatMemory.Messages))
	if b.config.DebugScreening {
		for i, msg := range chatMemory.Messages {
			InfoLogger.Printf("Message %d: IsUser=%v, Text=%q Images=%d", i, msg.IsUser, msg.Text, len(msg.ImageFileIDs))
		}
	}

	var contextMessages []anthropic.BetaMessageParam
	for _, msg := range chatMemory.Messages {
		blocks := contentBlocksForMessage(msg)
		if len(blocks) == 0 {
			continue
		}
		var param anthropic.BetaMessageParam
		if msg.IsUser {
			param = anthropic.NewBetaUserMessage(blocks...)
		} else {
			param = anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{
				Content: blocks,
			}}
		}
		contextMessages = append(contextMessages, param)
	}

	if b.config.CacheHistoryEnabled() {
		markTrailingCacheBreakpoint(contextMessages)
	}
	return contextMessages
}

// markTrailingCacheBreakpoint puts a cache_control breakpoint on the final
// content block of the conversation, so the next turn reads the whole prefix
// from cache instead of reprocessing it. The system prompt keeps its own
// breakpoint; tools and system render ahead of messages, so the two compose.
//
// Caveat worth knowing when reading [usage] lines: chat memory is a sliding
// window. Once it is full, each new turn evicts the oldest message, which
// changes the prefix and forces a miss. Until then, and for any chat shorter
// than the window, this converts a full-price reread into a cache read.
func markTrailingCacheBreakpoint(messages []anthropic.BetaMessageParam) {
	if len(messages) == 0 {
		return
	}
	var blocks []anthropic.BetaContentBlockParamUnion
	switch last := messages[len(messages)-1]; {
	case last.OfUser != nil:
		blocks = last.OfUser.Content
	case last.OfAssistant != nil:
		blocks = last.OfAssistant.Content
	}
	if len(blocks) == 0 {
		return
	}

	switch last := &blocks[len(blocks)-1]; {
	case last.OfText != nil:
		last.OfText.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	case last.OfImage != nil:
		last.OfImage.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}
}

func contentBlocksForMessage(msg Message) []anthropic.BetaContentBlockParamUnion {
	var blocks []anthropic.BetaContentBlockParamUnion
	if msg.IsUser && len(msg.ImageFileIDs) > 0 {
		multi := len(msg.ImageFileIDs) > 1
		for i, fileID := range msg.ImageFileIDs {
			if multi {
				blocks = append(blocks, anthropic.NewBetaTextBlock(fmt.Sprintf("Image %d:", i+1)))
			}
			blocks = append(blocks, anthropic.NewBetaImageBlock(anthropic.BetaFileImageSourceParam{FileID: fileID}))
		}
	}
	if textContent := strings.TrimSpace(msg.Text); textContent != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(textContent))
	}
	return blocks
}

func roleHasScope(role Role, scope string) bool {
	for _, s := range role.Scopes {
		if s.Name == scope {
			return true
		}
	}
	return false
}

func (b *Bot) hasScope(userID int64, scope string) bool {
	var user User
	if err := b.db.Preload("Role.Scopes").
		Where("telegram_id = ? AND bot_id = ?", userID, b.botID).
		First(&user).Error; err != nil {
		return false
	}
	if user.IsOwner {
		return true
	}
	return roleHasScope(user.Role, scope)
}

var publicBotCommands = []models.BotCommand{
	{Command: "stats", Description: "Get bot statistics. Usage: /stats or /stats user [user_id]"},
	{Command: "whoami", Description: "Get your user information"},
	{Command: "clear", Description: "Clear chat history (soft delete). Admins: /clear [user_id]"},
}

var adminBotCommands = []models.BotCommand{
	{Command: "clear_hard", Description: "Clear chat history (permanently delete). Admins: /clear_hard [user_id]"},
	{Command: "set_model", Description: "Switch the AI model (admin/owner only). Usage: /set_model <model-id>"},
}

func (b *Bot) registerAdminCommandsForUser(ctx context.Context, telegramID int64) {
	allCommands := make([]models.BotCommand, 0, len(publicBotCommands)+len(adminBotCommands))
	allCommands = append(allCommands, publicBotCommands...)
	allCommands = append(allCommands, adminBotCommands...)
	_, err := b.tgBot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: allCommands,
		Scope:    &models.BotCommandScopeChat{ChatID: telegramID},
	})
	if err != nil {
		ErrorLogger.Printf("Failed to register admin commands for user %d: %v", telegramID, err)
	}
}

func setElevatedCommands(tgBot TelegramClient, users []User) {
	allCommands := make([]models.BotCommand, 0, len(publicBotCommands)+len(adminBotCommands))
	allCommands = append(allCommands, publicBotCommands...)
	allCommands = append(allCommands, adminBotCommands...)
	for _, u := range users {
		if u.TelegramID == 0 {
			continue
		}
		if !u.IsOwner && !roleHasScope(u.Role, ScopeModelSet) {
			continue
		}
		_, err := tgBot.SetMyCommands(context.Background(), &bot.SetMyCommandsParams{
			Commands: allCommands,
			Scope:    &models.BotCommandScopeChat{ChatID: u.TelegramID},
		})
		if err != nil {
			ErrorLogger.Printf("Warning: could not set admin commands for user %d: %v", u.TelegramID, err)
		}
	}
}

func initTelegramBot(token string, b *Bot) (TelegramClient, error) {
	opts := []bot.Option{
		bot.WithDefaultHandler(b.handleUpdate),
	}

	tgBot, err := bot.New(token, opts...)
	if err != nil {
		return nil, err
	}

	_, err = tgBot.SetMyCommands(context.Background(), &bot.SetMyCommandsParams{
		Commands: publicBotCommands,
		Scope:    &models.BotCommandScopeDefault{},
	})
	if err != nil {
		ErrorLogger.Printf("Error setting default bot commands: %v", err)
		return nil, err
	}

	var allUsers []User
	if err := b.db.Preload("Role.Scopes").Where("bot_id = ?", b.botID).Find(&allUsers).Error; err != nil {
		ErrorLogger.Printf("Warning: could not query users for command scoping: %v", err)
	} else {
		setElevatedCommands(tgBot, allUsers)
	}

	return tgBot, nil
}

// sendResponse sends a notice that is not part of the conversation: a
// command's reply, a refusal, a failure the user is told about, anything sent
// outside a model turn. It is stored aside, so it never enters memory or the
// model's context, answers nothing, and cannot leave a pending message
// looking answered.
func (b *Bot) sendResponse(ctx context.Context, to replyTarget, text string) error {
	notice := b.createMessage(to.chatID, 0, "", "assistant", text, false)
	notice.Aside = true
	if err := b.storeMessage(&notice); err != nil {
		ErrorLogger.Printf("Error storing notice: %v", err)
		return err
	}
	return b.sendOneSegment(ctx, to, text)
}

// sendTurnReply records text as turn's reply, filed where the turn's context
// ended (recordTurnReply), then sends it.
func (b *Bot) sendTurnReply(ctx context.Context, turn *chatTurn, to replyTarget, text string) error {
	if _, err := b.recordTurnReply(turn, to.chatID, text); err != nil {
		ErrorLogger.Printf("Error storing assistant message: %v", err)
		return err
	}
	return b.sendOneSegment(ctx, to, text)
}

// setAside takes a stored user message out of the conversation when it is
// refused after intake. It never joined, so it is in no memory; only the row
// changes.
func (b *Bot) setAside(msg *Message) {
	msg.Aside = true
	if err := b.db.Model(&Message{}).Where("id = ?", msg.ID).Update("aside", true).Error; err != nil {
		ErrorLogger.Printf("[%s] setting message %d aside: %v", b.config.ID, msg.ID, err)
	}
}

func (b *Bot) sendOneSegment(ctx context.Context, to replyTarget, text string) error {
	params := &bot.SendMessageParams{
		ChatID:               to.chatID,
		MessageThreadID:      to.threadID,
		BusinessConnectionID: to.businessConnectionID,
		Text:                 text,
	}
	if _, err := b.tgBot.SendMessage(ctx, params); err != nil {
		ErrorLogger.Printf("[%s] Error sending segment to chat %d with BusinessConnectionID %s: %v",
			b.config.ID, to.chatID, to.businessConnectionID, err)
		return err
	}
	return nil
}

func (b *Bot) sendStats(ctx context.Context, to replyTarget, userID int64, targetUserID int64) {
	if targetUserID == 0 {
		totalUsers, totalMessages, err := b.getStats()
		if err != nil {
			ErrorLogger.Printf("Error fetching stats: %v\n", err)
			if err := b.sendResponse(ctx, to, "Sorry, I couldn't retrieve the stats at this time."); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}

		statsMessage := fmt.Sprintf(
			"📊 Bot Statistics:\n\n"+
				"- Total Users: %d\n"+
				"- Total Messages: %d",
			totalUsers,
			totalMessages,
		)

		if b.hasScope(userID, ScopeStatsViewAny) {
			type topEntry struct {
				UserID   int64
				MsgCount int64
			}
			var top []topEntry
			if err := b.db.Model(&Message{}).
				Select("user_id, COUNT(*) as msg_count").
				Where("bot_id = ? AND is_user = ? AND deleted_at IS NULL", b.botID, true).
				Group("user_id").
				Order("msg_count DESC").
				Limit(3).
				Scan(&top).Error; err != nil {
				ErrorLogger.Printf("Error fetching top users: %v", err)
			} else if len(top) > 0 {
				statsMessage += "\n\n🏆 Most Active Users:"
				for i, entry := range top {
					var u User
					if err := b.db.Select("username").Where("telegram_id = ? AND bot_id = ?", entry.UserID, b.botID).First(&u).Error; err != nil {
						u.Username = fmt.Sprintf("ID:%d", entry.UserID)
					}
					name := u.Username
					if name == "" {
						name = fmt.Sprintf("ID:%d", entry.UserID)
					}
					statsMessage += fmt.Sprintf("\n%d. @%s — %d messages", i+1, name, entry.MsgCount)
				}
			}
		}

		if err := b.sendResponse(ctx, to, statsMessage); err != nil {
			ErrorLogger.Printf("Error sending stats message: %v", err)
		}
		return
	}

	if targetUserID != userID {
		if !b.hasScope(userID, ScopeStatsViewAny) {
			InfoLogger.Printf("User %d attempted to view stats for user %d without permission", userID, targetUserID)
			if err := b.sendResponse(ctx, to, "Permission denied. Only admins and owners can view other users' statistics."); err != nil {
				ErrorLogger.Printf("Error sending response: %v", err)
			}
			return
		}
	}

	username, messagesIn, messagesOut, totalMessages, err := b.getUserStats(targetUserID)
	if err != nil {
		ErrorLogger.Printf("Error fetching user stats: %v\n", err)
		if err := b.sendResponse(ctx, to, fmt.Sprintf("Sorry, I couldn't retrieve statistics for user ID %d.", targetUserID)); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	userInfo := fmt.Sprintf("@%s (ID: %d)", username, targetUserID)
	if username == "" {
		userInfo = fmt.Sprintf("User ID: %d", targetUserID)
	}

	statsMessage := fmt.Sprintf(
		"👤 User Statistics for %s:\n\n"+
			"- Messages Sent: %d\n"+
			"- Messages Received: %d\n"+
			"- Total Messages: %d",
		userInfo,
		messagesIn,
		messagesOut,
		totalMessages,
	)

	if err := b.sendResponse(ctx, to, statsMessage); err != nil {
		ErrorLogger.Printf("Error sending user stats message: %v", err)
	}
}

func (b *Bot) getStats() (int64, int64, error) {
	var totalUsers int64
	if err := b.db.Model(&User{}).Where("bot_id = ?", b.botID).Count(&totalUsers).Error; err != nil {
		return 0, 0, err
	}

	var totalMessages int64
	if err := b.db.Model(&Message{}).Where("bot_id = ?", b.botID).Count(&totalMessages).Error; err != nil {
		return 0, 0, err
	}

	return totalUsers, totalMessages, nil
}

func (b *Bot) getUserStats(userID int64) (string, int64, int64, int64, error) {
	var user User
	err := b.db.Where("telegram_id = ? AND bot_id = ?", userID, b.botID).First(&user).Error
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("user not found: %w", err)
	}

	var messagesIn int64
	if err := b.db.Model(&Message{}).Where("user_id = ? AND bot_id = ? AND is_user = ?",
		userID, b.botID, true).Count(&messagesIn).Error; err != nil {
		return "", 0, 0, 0, err
	}

	var messagesOut int64
	if err := b.db.Model(&Message{}).Where("chat_id IN (SELECT DISTINCT chat_id FROM messages WHERE user_id = ? AND bot_id = ? AND deleted_at IS NULL) AND bot_id = ? AND is_user = ?",
		userID, b.botID, b.botID, false).Count(&messagesOut).Error; err != nil {
		return "", 0, 0, 0, err
	}

	totalMessages := messagesIn + messagesOut

	return user.Username, messagesIn, messagesOut, totalMessages, nil
}

func isOnlyEmojis(s string) bool {
	for _, r := range s {
		if !isEmoji(r) {
			return false
		}
	}
	return true
}

func isEmoji(r rune) bool {
	return (r >= 0x1F600 && r <= 0x1F64F) ||
		(r >= 0x1F300 && r <= 0x1F5FF) ||
		(r >= 0x1F680 && r <= 0x1F6FF) ||
		(r >= 0x2600 && r <= 0x26FF) ||
		(r >= 0x2700 && r <= 0x27BF)
}

func (b *Bot) sendWhoAmI(ctx context.Context, to replyTarget, userID int64, username string) {
	user, err := b.getOrCreateUser(userID, username, false)
	if err != nil {
		ErrorLogger.Printf("Error getting or creating user: %v", err)
		if err := b.sendResponse(ctx, to, "Sorry, I couldn't retrieve your information."); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	role, err := b.getRoleByName(user.Role.Name)
	if err != nil {
		ErrorLogger.Printf("Error getting role by name: %v", err)
		if err := b.sendResponse(ctx, to, "Sorry, I couldn't retrieve your role information."); err != nil {
			ErrorLogger.Printf("Error sending response: %v", err)
		}
		return
	}

	whoAmIMessage := fmt.Sprintf(
		"👤 Your Information:\n\n"+
			"- Username: %s\n"+
			"- Role: %s",
		user.Username,
		role.Name,
	)

	if err := b.sendResponse(ctx, to, whoAmIMessage); err != nil {
		ErrorLogger.Printf("Error sending /whoami message: %v", err)
	}
}

func (b *Bot) screenIncomingMessage(message *models.Message) (Message, error) {
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Incoming: chat=%d user=%d type=%s memory_size=%d duration=%v",
				message.Chat.ID,
				message.From.ID,
				messageType(message),
				len(b.getOrCreateChatMemory(message.Chat.ID).Messages),
				time.Since(start),
			)
		}()
	}

	userRole := "user"

	messageText := message.Text
	if message.Sticker != nil {
		if message.Sticker.Emoji != "" {
			messageText = fmt.Sprintf("Sent a sticker: %s", message.Sticker.Emoji)
		} else {
			messageText = "Sent a sticker."
		}
	}
	if message.Voice != nil {
		messageText = "[Voice message]"
	}

	userMessage := b.createMessage(message.Chat.ID, message.From.ID, message.From.Username, userRole, messageText, true)
	userMessage.TelegramMessageID = message.ID

	if message.Sticker != nil {
		userMessage.StickerFileID = message.Sticker.FileID
		userMessage.StickerEmoji = message.Sticker.Emoji
		if message.Sticker.Thumbnail != nil {
			userMessage.StickerPNGFile = message.Sticker.Thumbnail.FileID
		}
	}
	// A handled command and media the bot does not read never join the
	// conversation.
	userMessage.Aside = handledCommand(message) != "" || unreadMedia(message)

	// Intake only stores: a message joins memory once it is accepted into
	// the conversation (joinConversation). Memory is loaded first, or its
	// first load would read this row before the message joined.
	b.getOrCreateChatMemory(message.Chat.ID)
	if err := b.storeMessage(&userMessage); err != nil {
		return Message{}, err
	}
	return userMessage, nil
}

// recordTurnReply stores the bot's message for a turn. It goes into chat
// memory right after the newest message the turn's context included, not at
// the end: a message that arrived while the turn ran is not what this reply
// answers, and it stays after the reply, so the turn that answers it next ends
// with it, as the API requires, instead of with this reply. Only the user
// messages the turn covered are marked answered.
//
// Notices outside a model turn never come here: they are aside (sendResponse).
func (b *Bot) recordTurnReply(turn *chatTurn, chatID int64, response string) (Message, error) {
	frontier, covered := b.contextEnd(turn)
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Outgoing: chat=%d len=%d memory_size=%d duration=%v",
				chatID,
				len(response),
				len(b.getOrCreateChatMemory(chatID).Messages),
				time.Since(start),
			)
		}()
	}

	// Memory first: loaded after the store, it would read the reply from the
	// database and then file it a second time.
	chatMemory := b.getOrCreateChatMemory(chatID)
	assistantMessage := b.createMessage(chatID, 0, "", "assistant", response, false)
	if after, filedBefore := b.replyPredecessor(chatMemory, frontier); filedBefore {
		// Filed before messages that arrived during the turn: stamped just
		// after the message it follows, so a reload, which goes by
		// timestamp, keeps memory's order. A reply at the end keeps now.
		assistantMessage.Timestamp = after.Add(time.Microsecond)
	}
	if err := b.storeMessage(&assistantMessage); err != nil {
		return Message{}, err
	}
	b.markAnswered(chatID, b.fileReply(chatMemory, assistantMessage, frontier, covered))
	return assistantMessage, nil
}

// markAnswered records when the user messages with these ids were answered.
// A coalesced turn answers the whole batch, so every one of them is marked,
// not just the newest.
func (b *Bot) markAnswered(chatID int64, ids []uint) {
	if len(ids) == 0 {
		return
	}
	err := b.db.Model(&Message{}).
		Where("chat_id = ? AND bot_id = ? AND id IN ? AND answered_on IS NULL", chatID, b.botID, ids).
		Update("answered_on", time.Now()).Error
	if err != nil {
		ErrorLogger.Printf("Error marking user messages as answered: %v", err)
	}
}

func (b *Bot) promoteUserToAdmin(promoterID, userToPromoteID int64) error {
	if !b.hasScope(promoterID, ScopeUserPromote) {
		return errors.New("only admins or owners can promote users to admin")
	}

	userToPromote, err := b.getOrCreateUser(userToPromoteID, "", false)
	if err != nil {
		return err
	}

	var adminRole Role
	if err := b.db.Where("name = ?", "admin").First(&adminRole).Error; err != nil {
		return err
	}

	userToPromote.RoleID = adminRole.ID
	userToPromote.Role = adminRole
	if err := b.db.Save(&userToPromote).Error; err != nil {
		return err
	}

	b.registerAdminCommandsForUser(context.Background(), userToPromoteID)
	return nil
}

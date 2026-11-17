package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	chatMemories    map[convKey]*ChatMemory
	memorySize      int
	chatMemoriesMu  sync.RWMutex
	config          BotConfig
	userLimiters    map[int64]*userLimiter
	userLimitersMu  sync.RWMutex
	clock           Clock
	botID           uint
	albumBuffers    map[albumKey]*pendingAlbum
	albumBuffersMu  sync.Mutex
	intakeBuffers   map[convKey]*pendingIntake
	intakeBuffersMu sync.Mutex
	intakeSeq       uint64
	turns           map[convKey]*chatTurn
	turnsMu         sync.Mutex
	turnSeq         uint64

	// memorySeq is the last sequence given to a message in chat memory
	// (Message.Seq), and memoryGen the last generation given to a chat
	// memory (ChatMemory.generation). Guarded by chatMemoriesMu.
	memorySeq, memoryGen uint64

	// allChatsCutoffs holds, per user, the newest cutoff of an admin's
	// /clear of their messages in every chat (beginUserClear with chat 0):
	// a message of theirs at or below it was deleted, and never joins or
	// loads, in any chat, including one whose memory was not loaded then.
	// One entry per user so cleared, for the process's life; guarded by
	// chatMemoriesMu.
	allChatsCutoffs map[int64]uint

	// chatCutoffs holds, per chat ID and user, the newest cutoff of an
	// admin's /clear of that user's messages in that chat (/clear <user_id>
	// <chat_id>), which covers every conversation with that chat ID: each
	// Business connection's and each topic's, loaded or not. As
	// allChatsCutoffs, for the process's life; guarded by chatMemoriesMu.
	chatCutoffs map[chatUser]uint

	// commandLimiters holds each user's handled-command limit (allowCommand).
	// Guarded by userLimitersMu.
	commandLimiters map[int64]*commandLimiter

	// mediaWaiters holds each conversation's media turns waiting for it, in
	// no order (beginMediaTurn). Guarded by turnsMu.
	mediaWaiters map[convKey][]*mediaWaiter

	// textsOnWay holds, per conversation, the text messages that joined
	// memory but whose batch or turn is not on record yet (joinText,
	// landText). Guarded by intakeBuffersMu.
	textsOnWay map[convKey]*textsOnWay

	// username is the bot's Telegram username, from GetMe at startup: a
	// command addressed to a bot counts only when it names this one
	// (handledCommand). Set before updates are handled.
	username string

	// selfID is the bot's own Telegram user ID, from the same GetMe: a
	// Business message whose sender_business_bot is this bot is one of its
	// own replies, already recorded. 0 while unknown.
	selfID int64

	// ElevenLabs endpoints: fields so tests can point them at a stand-in.
	sttURL, ttsURL string

	// Drafts: the send throttle, the keepalive and how the sender sleeps
	// (fields so tests can shorten the timings and count the sender's timers).
	draftInterval  time.Duration
	draftKeepalive time.Duration
	draftAfter     func(time.Duration) <-chan time.Time

	// Progress-note status messages: the first-send delay and edit throttle
	// (fields so tests can shorten them), and the lines a shutdown waits for.
	statusFirstDelay   time.Duration
	statusEditInterval time.Duration
	statusLines        openStatusLines

	// businessConns caches what the bot knows of each Business connection
	// (businessConnection), businessConnSeq orders its stores, and
	// businessRefused holds the connections whose refusal was logged (B8).
	// Guarded by businessConnsMu.
	businessConns   map[string]businessConn
	businessConnSeq uint64
	businessRefused map[string]bool
	businessConnsMu sync.Mutex

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
		chatMemories:    make(map[convKey]*ChatMemory),
		memorySize:      config.MemorySize,
		config:          config,
		userLimiters:    make(map[int64]*userLimiter),
		commandLimiters: make(map[int64]*commandLimiter),
		allChatsCutoffs: make(map[int64]uint),
		chatCutoffs:     make(map[chatUser]uint),
		clock:           clock,
		botID:           botEntry.ID,
		tgBot:           tgClient,
		albumBuffers:    make(map[albumKey]*pendingAlbum),
		intakeBuffers:   make(map[convKey]*pendingIntake),
		turns:           make(map[convKey]*chatTurn),
		mediaWaiters:    make(map[convKey][]*mediaWaiter),
		textsOnWay:      make(map[convKey]*textsOnWay),

		draftInterval:  draftUpdateInterval,
		draftKeepalive: draftKeepaliveInterval,
		draftAfter:     time.After,

		statusFirstDelay:   defaultStatusFirstDelay,
		statusEditInterval: defaultStatusEditInterval,
		businessConns:      make(map[string]businessConn),
		businessRefused:    make(map[string]bool),

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

// createMessage is a new row for conversation k: its chat, connection and
// topic, which every row stores ("" and 0 where there are none).
func (b *Bot) createMessage(k convKey, userID int64, username, userRole, text string, isUser bool) Message {
	message := Message{
		UserRole:  userRole,
		Text:      text,
		Timestamp: time.Now(),
		IsUser:    isUser,
	}
	message.inConv(k)

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

// storePhotoArrival stores a photo or album message aside as it arrives,
// before its upload, as voice notes are stored: a /clear during the upload
// then covers it, and the photo does not join afterwards. The row carries
// its conversation, as a text's does.
func (b *Bot) storePhotoArrival(message *models.Message, to replyTarget) (Message, error) {
	from := senderOf(message)
	row := b.createMessage(to.conv(), from.id, from.username, "user", message.Caption, true)
	row.TelegramMessageID = message.ID
	row.Aside = true
	return row, b.storeMessage(&row)
}

// photoRowLive reports whether the photo's row stored at its arrival is still
// there and no clear's cutoff covers it, just before its upload. A failed
// read counts as live: recordPhotoUploads checks again.
func (b *Bot) photoRowLive(row Message) bool {
	var n int64
	if err := b.db.Model(&Message{}).Where("id = ?", row.ID).Count(&n).Error; err == nil && n == 0 {
		return false
	}
	b.getOrCreateChatMemory(row.conv())
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	return !b.deletedByClear(b.chatMemories[row.conv()], row)
}

// recordPhotoUploads writes the uploads, caption and Telegram id onto the
// photo's row stored at arrival, and reports whether the row is still there:
// a clear that ran during the upload deleted it.
func (b *Bot) recordPhotoUploads(row Message) (bool, error) {
	result := b.db.Model(&Message{Model: gorm.Model{ID: row.ID}}).
		Select("ImageFileIDs", "Text", "TelegramMessageID").
		Updates(&Message{ImageFileIDs: row.ImageFileIDs, Text: row.Text, TelegramMessageID: row.TelegramMessageID})
	return result.RowsAffected == 1, result.Error
}

func (b *Bot) getOrCreateChatMemory(k convKey) *ChatMemory {
	if chatMemory := b.loadedChatMemory(k); chatMemory != nil {
		return chatMemory
	}

	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	if chatMemory, exists := b.chatMemories[k]; exists {
		return chatMemory
	}
	history := b.loadChatHistory(k)
	b.memoryGen++
	chatMemory := &ChatMemory{Messages: make([]Message, 0, len(history)), Size: b.memorySize * 2, generation: b.memoryGen}
	for _, message := range history {
		if b.deletedEverywhere(message) {
			continue // an admin's clear of its sender's messages is deleting its row
		}
		b.insertLocked(chatMemory, message, len(chatMemory.Messages))
	}
	chatMemory.answeredThrough.Store(reloadedAnsweredThrough(chatMemory.Messages))
	b.chatMemories[k] = chatMemory
	return chatMemory
}

// loadedChatMemory is the conversation's memory if it is loaded, or nil.
func (b *Bot) loadedChatMemory(k convKey) *ChatMemory {
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	return b.chatMemories[k]
}

// loadChatHistory reads the conversation's newest messages from the
// database, oldest first, for a memory being created: at startup, or after
// /clear dropped it. Aside rows are skipped, and so are rows stored before
// rows carried their connection (convScope). Messages with the same
// timestamp keep the order they were stored in.
func (b *Bot) loadChatHistory(k convKey) []Message {
	var messages []Message
	scope, args := b.convScope(k)
	err := b.db.Where(scope, args...).Where("aside = ? OR aside IS NULL", false).
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
		if messages[i].IsUser || ownerSide(messages[i]) { // a business owner's message is no reply of the bot's
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

func (b *Bot) stripDeadFileIDFromMemory(k convKey, deadFileID string) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	cm, exists := b.chatMemories[k]
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

// joinText is joinConversation for a text message that goes on to a batch or
// a turn of its own: from its placement until that is on record (landText),
// it is on its way, and a media turn whose ticket came after it waits for it
// (G5). It is recorded in the same step under the memory lock as its
// placement, so any ticket taken after it sees it.
func (b *Bot) joinText(message Message) (held Message, outcome joinOutcome) {
	var seq uint64
	outcome = joinWriteFailed // until join returns: a panic inside it lands the text below
	defer func() {
		if outcome != joinOK && seq != 0 {
			b.landText(message.conv(), seq)
		}
	}()
	held, outcome = b.join(message, func(chatMemory *ChatMemory) (int, uint64) {
		b.memorySeq++
		seq = b.memorySeq
		b.intakeBuffersMu.Lock() // chatMemoriesMu, then intakeBuffersMu
		defer b.intakeBuffersMu.Unlock()
		b.markTextOnWayLocked(message.conv(), seq)
		return len(chatMemory.Messages), seq
	})
	return held, outcome
}

// joinConversation puts a stored user message into its chat's memory once it
// joins the conversation, and reports whether it did. Until then its row is
// aside: no turn can read, cover or answer it, and no reload can load it.
//
// Under the memory lock it takes its sequence and its join time as its
// timestamp, later than every message already in memory, so a reload keeps
// memory's order, and goes into memory; a copy already there (a row stored
// before rows waited aside) moves to the end with a fresh sequence. The row
// leaves aside after the lock is released, so a slow database stalls no other
// chat's memory. If that write finds the row gone (/clear deleted it), or
// /clear replaced the memory meanwhile, the message leaves memory again and
// joinConversation reports false: its path ends silently.
func (b *Bot) joinConversation(message Message) (Message, bool) {
	held, outcome := b.join(message, func(chatMemory *ChatMemory) (int, uint64) {
		return len(chatMemory.Messages), 0
	})
	return held, outcome == joinOK
}

// joinOutcome is how a join ended.
type joinOutcome uint8

const (
	joinOK          joinOutcome = iota
	joinCleared                 // a /clear deleted the message, or replaced its memory
	joinWriteFailed             // its row's write failed: the row stays aside
	joinNoRoom                  // a window of size 0
)

func (o joinOutcome) String() string {
	return [...]string{"joined", "its history was cleared", "its write failed", "the window was full"}[o]
}

// join is joinConversation with the slot in memory and the sequence (0 for
// the next) that place picks under the memory lock. The message is stamped
// between its neighbours there (replyStamp), so a reload keeps the order.
func (b *Bot) join(message Message, place func(*ChatMemory) (at int, seq uint64)) (Message, joinOutcome) {
	b.getOrCreateChatMemory(message.conv())
	chatMemory, held, outcome := b.placeJoining(message, place)
	if outcome != joinOK {
		InfoLogger.Printf("[%s] message %d in chat %v did not join: %s", b.config.ID, message.ID, message.conv(), outcome)
		return Message{}, outcome
	}
	if message.ID == 0 {
		return held, joinOK
	}

	// The row leaves aside only now that the message survived its placement.
	result := b.db.Model(&Message{}).Where("id = ?", message.ID).
		Updates(map[string]any{"aside": false, "timestamp": held.Timestamp})
	if result.Error != nil {
		ErrorLogger.Printf("[%s] joining message %d to chat %v: %v", b.config.ID, message.ID, message.conv(), result.Error)
	}
	if outcome := b.keepJoined(chatMemory, held, result); outcome != joinOK {
		InfoLogger.Printf("[%s] message %d in chat %v did not join: %s", b.config.ID, message.ID, message.conv(), outcome)
		return Message{}, outcome
	}
	return held, joinOK
}

// placeJoining is join's first step, under the memory lock: unless a /clear
// deleted message first, it goes into its chat's memory where place says,
// stamped between its neighbours. It returns that memory and the copy it
// holds, or why it did not join.
func (b *Bot) placeJoining(message Message, place func(*ChatMemory) (at int, seq uint64)) (*ChatMemory, Message, joinOutcome) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	chatMemory := b.chatMemories[message.conv()]
	if b.deletedByClear(chatMemory, message) {
		return nil, Message{}, joinCleared
	}
	if message.ID != 0 {
		chatMemory.Messages = slices.DeleteFunc(chatMemory.Messages, func(held Message) bool {
			return held.ID == message.ID
		})
	}
	message.Aside = false
	at, seq := place(chatMemory)
	message.Timestamp = b.replyStamp(chatMemory.Messages, at)
	held := b.placeLocked(chatMemory, message, at, seq)
	if !slices.ContainsFunc(chatMemory.Messages, func(m Message) bool { return m.Seq == held.Seq }) {
		return nil, Message{}, joinNoRoom // only for a window of size 0: a media message is pinned, text goes last
	}
	return chatMemory, held, joinOK
}

// keepJoined is join's second step, under the memory lock again, once the
// row's write is done. It returns why the message did not join, after taking
// it out of memory: the write failed (the row stays aside) or found the row
// gone (/clear deleted it), /clear replaced the memory meanwhile, or a clear
// of its sender's messages took it out. A text message the window trimmed
// while its write ran (a flood of later messages) did join: the conversation
// moved past it, as past any old message. A media message is never trimmed
// before its turn reads it (pinnedSeq).
func (b *Bot) keepJoined(chatMemory *ChatMemory, held Message, result *gorm.DB) joinOutcome {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	var refused joinOutcome
	switch {
	case result.Error != nil:
		refused = joinWriteFailed
	case result.RowsAffected == 0, b.chatMemories[held.conv()] != chatMemory, b.deletedByClear(chatMemory, held):
		refused = joinCleared
	case !slices.ContainsFunc(chatMemory.Messages, func(m Message) bool { return m.Seq == held.Seq }):
		InfoLogger.Printf("[%s] message %d in chat %v joined, and later messages already trimmed it out of the window",
			b.config.ID, held.ID, held.conv())
		return joinOK
	default:
		return joinOK
	}
	chatMemory.Messages = slices.DeleteFunc(chatMemory.Messages, func(m Message) bool { return m.Seq == held.Seq })
	return refused
}

// deletedByClear reports whether a /clear deleted message before it could
// join chatMemory: the clear that created the memory, a clear of its
// sender's messages in this conversation, in every conversation with its chat
// ID, or in every chat, up to that clear's cutoff. The caller holds
// chatMemoriesMu.
func (b *Bot) deletedByClear(chatMemory *ChatMemory, message Message) bool {
	return message.ID != 0 && (message.ID <= chatMemory.clearedThrough ||
		message.ID <= chatMemory.userCutoffs[message.UserID] || b.deletedEverywhere(message))
}

// deletedEverywhere reports whether an admin's clear of its sender's
// messages, in every chat or in every conversation with its chat ID, deleted
// message. The caller holds chatMemoriesMu.
func (b *Bot) deletedEverywhere(message Message) bool {
	return message.ID != 0 && (message.ID <= b.allChatsCutoffs[message.UserID] ||
		message.ID <= b.chatCutoffs[chatUser{chatID: message.ChatID, userID: message.UserID}])
}

// chatUser is one user in every conversation with one chat ID.
type chatUser struct {
	chatID, userID int64
}

// latestTimestamp is the latest Timestamp among messages, or the zero time.
func latestTimestamp(messages []Message) time.Time {
	var latest time.Time
	for _, m := range messages {
		if m.Timestamp.After(latest) {
			latest = m.Timestamp
		}
	}
	return latest
}

// stampAfter is now, or a microsecond after latest when the clock has not
// moved past it (one clock tick, or a clock set back), so stamps given in
// memory order sort in that order.
func (b *Bot) stampAfter(latest time.Time) time.Time {
	now := wallUTC(b.clock.Now())
	if next := latest.Add(time.Microsecond); now.Before(next) {
		return wallUTC(next)
	}
	return now
}

// wallUTC is t as a conversation stamp: wall-clock UTC with the monotonic
// reading stripped. The database sorts stamps as text, so they share one
// offset; and Go compares two times by their monotonic readings when both
// carry one, which, after the wall clock is set back, contradicts the wall
// times the database sorts by.
func wallUTC(t time.Time) time.Time {
	return t.Round(0).UTC()
}

// insertLocked puts message into memory at index at with the next sequence
// number, then trims the window from the front, and returns the copy memory
// holds. Sequences only grow, across chats and reloads, so one taken before a
// message was inserted is always below that message's. The caller holds
// chatMemoriesMu for writing.
func (b *Bot) insertLocked(chatMemory *ChatMemory, message Message, at int) Message {
	return b.placeLocked(chatMemory, message, at, 0)
}

// placeLocked is insertLocked with the sequence seq reserved for the message
// earlier (reserveSeq), or the next one for 0.
func (b *Bot) placeLocked(chatMemory *ChatMemory, message Message, at int, seq uint64) Message {
	if seq == 0 {
		b.memorySeq++
		seq = b.memorySeq
	}
	message.Seq = seq
	chatMemory.Messages = slices.Insert(chatMemory.Messages, at, message)
	trimLocked(chatMemory)
	return message
}

// trimLocked drops the oldest messages beyond the window, but never the media
// message pinned until its turn reads it (pinnedSeq, set before it is
// placed): a media message that waited for the chat can be placed at the
// front of a window full of later text. A reply placed at the front, because
// the window moved past its context, is still trimmed at once. The caller
// holds chatMemoriesMu.
func trimLocked(chatMemory *ChatMemory) {
	pinned := chatMemory.pinnedSeq.Load()
	for len(chatMemory.Messages) > chatMemory.Size {
		oldest := slices.IndexFunc(chatMemory.Messages, func(m Message) bool { return pinned == 0 || m.Seq != pinned })
		if oldest < 0 {
			return
		}
		chatMemory.Messages = slices.Delete(chatMemory.Messages, oldest, oldest+1)
	}
}

// fileReply stores a turn's reply and puts it into memory right after the
// last message at or below frontier, or at the end when frontier is 0, and
// raises the chat's answeredThrough to covered. Messages that arrived while
// the turn ran have higher sequences, so they stay after the reply. It
// returns the database ids of the user messages the reply newly answers.
//
// The reply is stamped, stored and inserted in one step under the memory
// lock, so nothing can join between choosing its slot and its timestamp: a
// reload, which goes by timestamp, then keeps memory's order (replyStamp).
//
// A turn that read a memory /clear has since replaced (memoryGen no longer
// the chat's) answers a history that is gone: its reply is not stored at all,
// so the clear's cutoff stays exact, and errStaleReply tells the caller not
// to send it either.
//
// A turn a clear of one sender's messages stopped (cancelClear) files
// nothing either, with errStaleReply; one whose reply that clear wants
// aside (replyAside) has it stored aside instead, and not put into memory.
// What it answers counts as answered all the same. Both are read under the
// lock here (clearFlags).
func (b *Bot) fileReply(chatMemory *ChatMemory, reply *Message, frontier, covered, memoryGen uint64, turn *chatTurn) ([]uint, error) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()

	stale := memoryGen != 0 && (memoryGen != chatMemory.generation || b.chatMemories[reply.conv()] != chatMemory)
	cleared, aside := b.clearFlags(turn)
	if stale || cleared {
		InfoLogger.Printf("[%s] reply for chat %v dropped: the history it answers was cleared", b.config.ID, reply.conv())
		return nil, errStaleReply
	}
	at := replyIndex(chatMemory, frontier)
	reply.Timestamp = b.replyStamp(chatMemory.Messages, at)
	reply.Aside = aside
	if err := b.storeMessage(reply); err != nil {
		return nil, err
	}
	// Every user message the reply covers, those answered before included
	// (markAnswered skips them): a media message joins with the sequence of
	// its ticket, which may be below messages answered while it waited.
	before := chatMemory.answeredThrough.Load()
	var answered []uint
	for _, m := range chatMemory.Messages {
		if m.IsUser && m.ID != 0 && m.Seq <= covered {
			answered = append(answered, m.ID)
		}
	}
	if !aside {
		b.insertLocked(chatMemory, *reply, at)
	}
	if covered > before {
		chatMemory.answeredThrough.Store(covered)
	}
	return answered, nil
}

// clearFlags reads, under turnsMu, whether a clear stopped turn (cancelClear)
// or wants its reply aside (replyAside). fileReply calls it holding
// chatMemoriesMu, the order the clears set them in, so no clear can land
// between the check and the filing.
func (b *Bot) clearFlags(turn *chatTurn) (cleared, aside bool) {
	if turn == nil {
		return false, false
	}
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	return turn.reason == cancelClear, turn.replyAside
}

// replyStamp is the timestamp for a reply going in at index at of messages:
// a microsecond after the latest message before its slot, kept before the
// earliest message after it (halfway, when they are a microsecond or less
// apart); at the end, now, or after the latest message if the clock has not
// passed it; at the front, a microsecond before the first. Messages join with
// increasing stamps (joinConversation), so the slot always has room.
func (b *Bot) replyStamp(messages []Message, at int) time.Time {
	if at == len(messages) {
		return b.stampAfter(latestTimestamp(messages))
	}
	next := messages[at].Timestamp
	for _, m := range messages[at:] {
		if m.Timestamp.Before(next) {
			next = m.Timestamp
		}
	}
	if at == 0 {
		return wallUTC(next.Add(-time.Microsecond))
	}
	latest := latestTimestamp(messages[:at])
	if stamp := latest.Add(time.Microsecond); stamp.Before(next) {
		return wallUTC(stamp)
	}
	return wallUTC(latest.Add(next.Sub(latest) / 2))
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
//
// unanswered reports whether the context holds a user message no reply
// covers yet. A /clear of one sender's messages can take away all a turn was
// for before it reads memory; that turn then ends silently.
func (b *Bot) snapshotForTurn(turn *chatTurn, chatMemory *ChatMemory) (contextMessages []anthropic.BetaMessageParam, unanswered bool) {
	messages, unanswered := b.readForTurn(turn, chatMemory)
	return b.buildContextMessages(messages), unanswered
}

// readForTurn copies chatMemory's messages and records on turn what they
// cover, in one step under the memory lock, taking turnsMu inside it, the
// established order: a clear, which applies itself to the turn under the
// memory lock, lands before the copy, which then lacks its messages, or after
// the record, which it then sees (blind r5b #2). The context is built from
// the copy outside the lock. The copy ends with the newest user message, as
// every request must (I1): what follows it is a business owner's message
// (recordOwnerMessage), which came after what the turn answers, and stays
// after its reply, as a message that lands while the turn runs does.
func (b *Bot) readForTurn(turn *chatTurn, chatMemory *ChatMemory) (messages []Message, unanswered bool) {
	snapshotStarting(turn)
	defer snapshotTaken(turn) // once the lock below is released
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	end := len(chatMemory.Messages)
	for end > 0 && ownerSide(chatMemory.Messages[end-1]) {
		end--
	}
	messages = slices.Clone(chatMemory.Messages[:end])
	covered := newestAnswerableSeq(messages)
	var frontier uint64
	for _, m := range messages {
		frontier = max(frontier, m.Seq)
	}
	b.markSnapshot(turn, covered, frontier, chatMemory.generation)
	return messages, covered > chatMemory.answeredThrough.Load()
}

// beforeSnapshot and afterSnapshot, when set, run right before a turn's
// snapshot step takes the memory lock, and right after it releases it: the
// last point a clear can reach a turn before it reads memory, and the
// earliest once it has. Tests land one there; production leaves them nil.
var beforeSnapshot, afterSnapshot atomic.Pointer[func(*chatTurn)]

// snapshotStarting runs beforeSnapshot, if set, for turn.
func snapshotStarting(turn *chatTurn) {
	if hook := beforeSnapshot.Load(); hook != nil {
		(*hook)(turn)
	}
}

// snapshotTaken runs afterSnapshot, if set, for turn.
func snapshotTaken(turn *chatTurn) {
	if hook := afterSnapshot.Load(); hook != nil {
		(*hook)(turn)
	}
}

// newestAnswerableSeq is the highest sequence among the answerable user
// messages, or 0. The caller holds chatMemoriesMu.
func newestAnswerableSeq(messages []Message) uint64 {
	var newest uint64
	for _, msg := range messages {
		if answerable(msg) {
			newest = max(newest, msg.Seq)
		}
	}
	return newest
}

// newestAnswerableSeqInChat is what a text flush for the conversation wants
// answered.
func (b *Bot) newestAnswerableSeqInChat(k convKey) uint64 {
	want, _ := b.flushWant(k)
	return want
}

// flushWant is what a text flush for the conversation wants answered, and
// the generation of the memory it read that from. A flush that reaches a
// turn in another generation (/clear replaced the memory meanwhile) is
// dropped.
func (b *Bot) flushWant(k convKey) (wantSeq, memoryGen uint64) {
	b.getOrCreateChatMemory(k)
	b.chatMemoriesMu.RLock()
	defer b.chatMemoriesMu.RUnlock()
	chatMemory := b.chatMemories[k]
	return newestAnswerableSeq(chatMemory.Messages), chatMemory.generation
}

// currentGeneration is the generation of the conversation's memory.
func (b *Bot) currentGeneration(k convKey) uint64 {
	_, gen := b.flushWant(k)
	return gen
}

// beginClear empties conversation k for /clear or /clear_hard, in one
// step under the memory lock: it records the highest message id as the
// clear's cutoff, swaps in an empty memory with a fresh generation (a swap,
// not a delete, so no reload before the rows are gone can bring them back),
// cancels its running turn for good (cancelClear), and drops its waiting
// batch: otherwise its quiet window would end moments later
// and answer the very messages being deleted (the openclaw/openclaw#51046
// failure mode, with a privacy cost), while a batch of a message sent after
// this step is kept. It takes the locks in the established order,
// chatMemoriesMu, then turnsMu, then intakeBuffersMu. The caller then
// deletes the scope's rows up to the cutoff.
//
// A reply filed into the old memory before this step has an id at or below
// the cutoff and is deleted with the rest; one filed after finds its memory
// replaced and is neither stored nor sent (fileReply). A message stored
// before it never joins the new memory (joinConversation).
func (b *Bot) beginClear(k convKey) (uint, error) {
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	cutoff, err := b.clearCutoff()
	if err != nil {
		return 0, err
	}
	b.memoryGen++
	b.chatMemories[k] = &ChatMemory{
		Messages: []Message{}, Size: b.memorySize * 2, generation: b.memoryGen, clearedThrough: cutoff,
	}
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if running, exists := b.turns[k]; exists {
		running.reason = cancelClear
		running.rerun, running.rerunWant, running.rerunFirst = nil, 0, 0
		running.cancel()
	}
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	if pending := b.removeIntakeLocked(k); pending != nil {
		InfoLogger.Printf("[%s] discarded %d buffered message(s) for chat %v on history clear",
			b.config.ID, pending.count, k)
	}
	return cutoff, nil
}

// clearCutoff is the highest message id stored so far, deleted or not: a
// clear that starts now deletes its scope's rows up to it. The caller holds
// chatMemoriesMu.
func (b *Bot) clearCutoff() (uint, error) {
	var cutoff uint
	if err := b.db.Unscoped().Model(&Message{}).Select("COALESCE(MAX(id), 0)").Scan(&cutoff).Error; err != nil {
		return 0, fmt.Errorf("reading the clear's cutoff: %w", err)
	}
	return cutoff, nil
}

// userClear is the scope of a clear of one user's own messages: one
// conversation (conv, an own /clear), every conversation with one chat ID
// (an admin's /clear <user_id> <chat_id>: each Business connection's and
// each topic's), or every conversation (an admin's /clear <user_id>, chatID
// 0).
type userClear struct {
	userID int64
	conv   *convKey
	chatID int64
}

// covers reports whether the clear reaches conversation k.
func (c userClear) covers(k convKey) bool {
	switch {
	case c.conv != nil:
		return k == *c.conv
	case c.chatID != 0:
		return k.chatID == c.chatID
	default:
		return true
	}
}

// rows is the database scope of the user's rows the clear deletes. The chat
// and all-chats forms name no connection: they reach rows stored before rows
// carried one, too.
func (c userClear) rows(b *Bot) (string, []any) {
	switch {
	case c.conv != nil:
		scope, args := b.convScope(*c.conv)
		return scope + " AND user_id = ?", append(args, c.userID)
	case c.chatID != 0:
		return "bot_id = ? AND chat_id = ? AND user_id = ?", []any{b.botID, c.chatID, c.userID}
	default:
		return "bot_id = ? AND user_id = ?", []any{b.botID, c.userID}
	}
}

// beginUserClear takes the user's own messages out of every conversation in
// memory the clear covers, for a /clear of only their messages, in one step
// under the memory lock: it records the highest message id as that clear's
// cutoff, and removes their messages from memory. The cutoff stays in force
// (deletedByClear): a message of theirs stored before it never joins
// (joinConversation), not even for the moment its row's write takes, and
// none loads into a memory first loaded later (getOrCreateChatMemory). Each
// memory, its generation and its turn stay: the conversation goes on for
// everyone else. The caller then deletes the sender's rows up to the cutoff.
//
// An own clear's cutoff is kept with its conversation's memory
// (ChatMemory.userCutoffs), the newest per sender, which is loaded first for
// that; an admin's is kept bot-wide, one per user and chat ID (chatCutoffs)
// or one per user for every chat (allChatsCutoffs), as it also covers
// conversations not loaded yet. A failed delete leaves nothing to undo: the
// rows it missed stay out of memory.
func (b *Bot) beginUserClear(c userClear) (uint, error) {
	if c.conv != nil {
		b.getOrCreateChatMemory(*c.conv)
	}
	b.chatMemoriesMu.Lock()
	defer b.chatMemoriesMu.Unlock()
	cutoff, err := b.clearCutoff()
	if err != nil {
		return 0, err
	}
	switch {
	case c.conv != nil:
	case c.chatID != 0:
		if b.chatCutoffs == nil {
			b.chatCutoffs = make(map[chatUser]uint)
		}
		b.chatCutoffs[chatUser{chatID: c.chatID, userID: c.userID}] = cutoff
	default:
		if b.allChatsCutoffs == nil {
			b.allChatsCutoffs = make(map[int64]uint)
		}
		b.allChatsCutoffs[c.userID] = cutoff
	}
	for k, chatMemory := range b.chatMemories {
		if !c.covers(k) {
			continue
		}
		if c.conv != nil {
			if chatMemory.userCutoffs == nil {
				chatMemory.userCutoffs = make(map[int64]uint)
			}
			chatMemory.userCutoffs[c.userID] = cutoff
		}
		b.stopForUserClearLocked(k, chatMemory, c.userID, cutoff)
		chatMemory.Messages = slices.DeleteFunc(chatMemory.Messages, func(m Message) bool {
			return m.UserID == c.userID && m.ID <= cutoff
		})
	}
	return cutoff, nil
}

// stopForUserClearLocked applies a clear of userID's messages up to cutoff to
// conversation k's running turn and waiting batch, in the clear's one step,
// before the messages leave memory:
//
//   - a turn whose unanswered messages are all theirs ends silently
//     (cancelClear);
//   - a media turn whose own media message is theirs ends silently too, the
//     others' messages it covered collected behind it, or, having shown part
//     of its answer, finishes with its reply stored aside
//     (stopMediaForUserClearLocked);
//   - one that also answers others restarts if it has shown nothing, reading
//     memory again without their messages, or, committed, goes on with its
//     reply stored aside (replyAside);
//   - work collected behind the turn, and a waiting batch, that hold only
//     their messages are dropped. Mixed work stays, and reads memory without
//     their messages.
//
// The caller holds chatMemoriesMu; it takes turnsMu, then intakeBuffersMu.
func (b *Bot) stopForUserClearLocked(k convKey, chatMemory *ChatMemory, userID int64, cutoff uint) {
	answered := chatMemory.answeredThrough.Load()
	senders := func(from, to uint64) (theirs, others bool) {
		for _, m := range chatMemory.Messages {
			if !m.IsUser || m.Seq <= answered || m.Seq < from || m.Seq > to {
				continue
			}
			if m.UserID == userID && m.ID <= cutoff {
				theirs = true
			} else {
				others = true
			}
		}
		return theirs, others
	}
	b.turnsMu.Lock()
	defer b.turnsMu.Unlock()
	if running := b.turns[k]; running != nil && running.reason != cancelClear {
		if running.rerun != nil {
			if theirs, others := senders(running.rerunFirst, running.rerunWant); theirs && !others {
				running.rerun, running.rerunWant, running.rerunFirst = nil, 0, 0
			}
		}
		switch theirs, others := senders(0, running.coveredSeq); {
		case b.stopMediaForUserClearLocked(chatMemory, running, userID, cutoff):
		case !theirs:
		case !others:
			InfoLogger.Printf("[%s] turn %d for chat %v stopped: it answers only cleared messages", b.config.ID, running.seq, k)
			running.reason, running.coveredSeq = cancelClear, 0 // it answers nothing now
			running.cancel()
		case !running.committed && running.req.ctx != nil:
			InfoLogger.Printf("[%s] turn %d for chat %v restarting without cleared messages", b.config.ID, running.seq, k)
			if running.rerun == nil {
				req := running.req
				running.rerun, running.rerunWant = &req, running.coveredSeq
			}
			running.rerunFirst = earliest(running.rerunFirst, running.firstSeq)
			running.reason = cancelRestart
			running.cancel()
		default:
			running.replyAside = true
		}
	}
	b.intakeBuffersMu.Lock()
	defer b.intakeBuffersMu.Unlock()
	if pending := b.intakeBuffers[k]; pending != nil && !pending.mixed && pending.from.userID == userID {
		b.removeIntakeLocked(k)
	}
}

// stopMediaForUserClearLocked applies a clear of userID's messages up to
// cutoff to running when it is a media turn answering one of them, its own
// media message, whatever answeredThrough says (a media message joins with
// its ticket, which can be below it): having shown nothing, the turn ends
// silently, and the other senders' unanswered messages before its media
// message, which it would have covered, are collected behind it, to be
// answered by a text turn; having shown part of its answer, or begun filing
// its reply (shown), it finishes, its reply stored aside (replyAside). The media message is known by the sender
// and row the turn recorded at its admission, not looked up in memory: once
// the turn read it, later messages can trim it out of the window (scoped r6
// #5). It reports whether running was such a turn. The caller holds
// chatMemoriesMu, then turnsMu.
func (b *Bot) stopMediaForUserClearLocked(chatMemory *ChatMemory, running *chatTurn, userID int64, cutoff uint) bool {
	theirs := func(m Message) bool { return m.UserID == userID && m.ID <= cutoff }
	if running.mediaSeq == 0 || running.mediaRow == 0 || running.mediaFrom != userID || running.mediaRow > cutoff {
		return false
	}
	// What the turn reads: the messages before its own (its snapshot may not
	// be recorded yet). Once the window trimmed its message, every message
	// before it went first; before it joined, those with an earlier sequence.
	before := chatMemory.Messages
	if at := slices.IndexFunc(chatMemory.Messages, func(m Message) bool { return m.Seq == running.mediaSeq }); at >= 0 {
		before = chatMemory.Messages[:at]
	} else {
		before = slices.DeleteFunc(slices.Clone(before), func(m Message) bool { return m.Seq > running.mediaSeq })
	}
	if running.shown {
		running.replyAside = true
		return true
	}
	InfoLogger.Printf("[%s] media turn %d for chat %v stopped: its message was cleared", b.config.ID, running.seq, running.conv)
	answered := chatMemory.answeredThrough.Load()
	var first, want uint64
	var oldest Message
	for _, m := range before {
		if answerable(m) && !theirs(m) && m.Seq > answered {
			if first == 0 || m.Seq < first {
				oldest = m
			}
			first, want = earliest(first, m.Seq), max(want, m.Seq)
		}
	}
	if want != 0 {
		// The rerun answers others' messages in the media turn's place: its
		// conversation and reply target are the media turn's, and its sender
		// is the oldest message's it answers (scoped r6 nit 7), as far as a
		// stored message tells it.
		req := running.req
		req.userID, req.username, req.messageTime = oldest.UserID, oldest.Username, int(oldest.Timestamp.Unix())
		collectLocked(running, &req, want, first)
	}
	running.reason, running.coveredSeq = cancelClear, 0 // it answers nothing now
	running.cancel()
	return true
}

// replyIsStale reports whether turn read a memory /clear has since replaced,
// so whatever it would send answers a history that is gone.
func (b *Bot) replyIsStale(turn *chatTurn) bool {
	_, _, memoryGen := b.contextEnd(turn)
	return memoryGen != 0 && memoryGen != b.currentGeneration(turn.conv)
}

// buildContextMessages converts memory messages, a copy taken under
// chatMemoriesMu, into API messages.
func (b *Bot) buildContextMessages(messages []Message) []anthropic.BetaMessageParam {
	InfoLogger.Printf("Chat memory contains %d messages", len(messages))
	if b.config.DebugScreening {
		for i, msg := range messages {
			InfoLogger.Printf("Message %d: IsUser=%v, Text=%q Images=%d", i, msg.IsUser, msg.Text, len(msg.ImageFileIDs))
		}
	}

	var contextMessages []anthropic.BetaMessageParam
	for _, msg := range messages {
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
		bot.WithSkipGetMe(), // called below, to learn the bot's username
	}

	tgBot, err := bot.New(token, opts...)
	if err != nil {
		return nil, err
	}
	getMeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	me, err := tgBot.GetMe(getMeCtx)
	if err != nil {
		return nil, fmt.Errorf("error call getMe, %w", err)
	}
	b.username, b.selfID = me.Username, me.ID

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
	notice := b.createMessage(to.conv(), 0, "", "assistant", text, false)
	notice.Aside = true
	if err := b.storeMessage(&notice); err != nil {
		ErrorLogger.Printf("Error storing notice: %v", err)
		return err
	}
	return b.sendOneSegment(ctx, to, text)
}

// noticeJoinFailed tells the sender when their message could not be saved
// into the conversation (its row's write failed), as a failed store does; a
// message a /clear removed ends silently.
func (b *Bot) noticeJoinFailed(ctx context.Context, to replyTarget, outcome joinOutcome) {
	if outcome == joinWriteFailed {
		b.sendNotice(ctx, to, "Sorry, I had trouble saving your message.")
	}
}

// sendNotice is sendResponse for a caller that can only log a failure.
func (b *Bot) sendNotice(ctx context.Context, to replyTarget, text string) {
	if err := b.sendResponse(ctx, to, text); err != nil {
		ErrorLogger.Printf("Error sending response: %v", err)
	}
}

// sendTurnReply records text as turn's reply, filed where the turn's context
// ended (recordTurnReply), then sends it. A reply to a history /clear has
// replaced is neither recorded nor sent.
func (b *Bot) sendTurnReply(ctx context.Context, turn *chatTurn, to replyTarget, text string) error {
	if _, err := b.recordTurnReply(turn, to.conv(), text); errors.Is(err, errStaleReply) {
		return nil
	} else if err != nil {
		ErrorLogger.Printf("Error storing assistant message: %v", err)
		return err
	}
	return b.sendOneSegment(ctx, to, text)
}

func (b *Bot) sendOneSegment(ctx context.Context, to replyTarget, text string) error {
	if b.connectionStopped(to.businessConnectionID) {
		ErrorLogger.Printf("[%s] not sending to chat %d: business connection %s %v", b.config.ID, to.chatID, to.businessConnectionID, errConnectionStopped)
		return errConnectionStopped
	}
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
				Where("bot_id = ? AND (is_user = ? OR user_role = ?) AND deleted_at IS NULL", b.botID, true, roleBusinessOwner).
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

	// A business owner's own messages, kept as the business side of the
	// conversation (recordOwnerMessage), count as what they sent, as they
	// did when they were stored as user messages, and not as a reply anyone
	// received from the bot.
	var messagesIn int64
	if err := b.db.Model(&Message{}).Where("user_id = ? AND bot_id = ? AND (is_user = ? OR user_role = ?)",
		userID, b.botID, true, roleBusinessOwner).Count(&messagesIn).Error; err != nil {
		return "", 0, 0, 0, err
	}

	var messagesOut int64
	if err := b.db.Model(&Message{}).Where("chat_id IN (SELECT DISTINCT chat_id FROM messages WHERE user_id = ? AND bot_id = ? AND deleted_at IS NULL) AND bot_id = ? AND is_user = ? AND (user_role IS NULL OR user_role <> ?)",
		userID, b.botID, b.botID, false, roleBusinessOwner).Count(&messagesOut).Error; err != nil {
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

func (b *Bot) screenIncomingMessage(message *models.Message, to replyTarget) (Message, error) {
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Incoming: chat=%d user=%d type=%s memory_size=%d duration=%v",
				message.Chat.ID,
				message.From.ID,
				messageType(message),
				len(b.getOrCreateChatMemory(to.conv()).Messages),
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

	from := senderOf(message)
	userMessage := b.createMessage(to.conv(), from.id, from.username, userRole, messageText, true)
	userMessage.TelegramMessageID = message.ID

	if message.Sticker != nil {
		userMessage.StickerFileID = message.Sticker.FileID
		userMessage.StickerEmoji = message.Sticker.Emoji
		if message.Sticker.Thumbnail != nil {
			userMessage.StickerPNGFile = message.Sticker.Thumbnail.FileID
		}
	}
	// Intake only stores, aside: the message joins the conversation, and
	// leaves aside, once it is accepted (joinConversation). A handled
	// command, a rate-limited message, a refused or untranscribed voice note
	// and media the bot does not read never join, and stay aside.
	userMessage.Aside = true
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
func (b *Bot) recordTurnReply(turn *chatTurn, k convKey, response string) (Message, error) {
	if turn != nil {
		b.markCommitted(turn) // about to be filed and sent: a clear from here on sets it aside
	}
	frontier, covered, memoryGen := b.contextEnd(turn)
	if b.config.DebugScreening {
		start := time.Now()
		defer func() {
			InfoLogger.Printf(
				"[Screen] Outgoing: chat=%v len=%d memory_size=%d duration=%v",
				k,
				len(response),
				len(b.getOrCreateChatMemory(k).Messages),
				time.Since(start),
			)
		}()
	}

	// Memory first: loaded after the store, it would read the reply from the
	// database and then file it a second time.
	chatMemory := b.getOrCreateChatMemory(k)
	assistantMessage := b.createMessage(k, 0, "", "assistant", response, false)
	answered, err := b.fileReply(chatMemory, &assistantMessage, frontier, covered, memoryGen, turn)
	if err != nil {
		return Message{}, err
	}
	b.markAnswered(k, answered)
	return assistantMessage, nil
}

// markAnswered records when the user messages with these ids were answered.
// A coalesced turn answers the whole batch, so every one of them is marked,
// not just the newest.
func (b *Bot) markAnswered(k convKey, ids []uint) {
	if len(ids) == 0 {
		return
	}
	scope, args := b.convScope(k)
	err := b.db.Model(&Message{}).Where(scope, args...).
		Where("id IN ? AND answered_on IS NULL", ids).
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

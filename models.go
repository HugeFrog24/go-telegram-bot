package main

import (
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

type BotModel struct {
	gorm.Model
	Identifier string `gorm:"uniqueIndex"`
	Name       string
	Configs    []ConfigModel `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
	Users      []User        `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
	Messages   []Message     `gorm:"foreignKey:BotID;constraint:OnDelete:CASCADE"`
}

type ConfigModel struct {
	gorm.Model
	BotID           uint   `gorm:"index"`
	MemorySize      int    `json:"memory_size"`
	MessagePerHour  int    `json:"messages_per_hour"`
	MessagePerDay   int    `json:"messages_per_day"`
	TempBanDuration string `json:"temp_ban_duration"`
	SystemPrompts   string `json:"system_prompts"`
	TelegramToken   string `json:"telegram_token"`
	Active          bool   `json:"active"`
}

type Message struct {
	gorm.Model
	BotID  uint  `gorm:"index;index:idx_messages_conversation,priority:1"`
	ChatID int64 `gorm:"index;index:idx_messages_conversation,priority:3"`
	// BusinessConnectionID is the Business connection the message came
	// through or went out on, "" for a direct or group chat: with ChatID and
	// ThreadID it names the message's conversation (convKey). NULL on rows
	// stored before it was, which no conversation loads again (convScope).
	BusinessConnectionID string `gorm:"index;index:idx_messages_conversation,priority:2"`
	UserID               int64  `gorm:"index"`
	Username             string `gorm:"index"`
	UserRole             string
	Text                 string    `gorm:"type:text"`
	Timestamp            time.Time `gorm:"index"`
	IsUser               bool
	StickerFileID        string
	StickerPNGFile       string
	StickerEmoji         string
	DeletedAt            gorm.DeletedAt `gorm:"index"`
	AnsweredOn           *time.Time     `gorm:"index"`
	ImageFileIDs         []string       `gorm:"type:text;serializer:json"`
	FilesCleanedAt       *time.Time     `gorm:"index"`
	// TelegramMessageID is Telegram's message_id, kept so an edited_message
	// update can find and correct the stored copy. Zero on rows stored before
	// edits were tracked.
	TelegramMessageID int `gorm:"index"`
	// Seq is the message's place in chat memory: a number given when it is
	// put there, higher than every number given before, so a message added
	// after a turn read memory has a higher one than all the turn read. It
	// lives in memory only; a reload gives new ones in load order.
	Seq uint64 `gorm:"-"`
	// ThreadID is the forum topic of the message's conversation (convKey), 0
	// outside a forum supergroup's topics. Stored, as a reload of one topic's
	// conversation loads only its rows; NULL on rows stored before it was.
	ThreadID int
	// Aside keeps a stored message out of the conversation: a handled
	// command and its reply, a rate-limited message and its notice, a voice
	// note refused or not transcribed and its notice, media the bot does not
	// read, and any other notice sent outside a model turn. The row stays
	// for stats and the record, but never enters chat memory or the model's
	// context, answers nothing, and a reload skips it.
	Aside bool `gorm:"default:false"`
}

type ChatMemory struct {
	Messages []Message
	Size     int

	// generation tells this memory from the one /clear replaced: a turn files
	// its reply only into the memory it read (fileReply), and a flush only
	// runs in the memory it read its want from. Set once, when the memory is
	// created.
	generation uint64

	// clearedThrough is the highest message id the /clear that created this
	// memory deleted (beginClear), 0 for a memory loaded from the database.
	// A message at or below it predates the clear and never joins.
	clearedThrough uint

	// userCutoffs holds, per sender, the newest cutoff of a /clear of their
	// own messages in this chat (beginUserClear): a message of theirs at or
	// below it was deleted, and never joins. Kept as long as the memory,
	// like clearedThrough; guarded by chatMemoriesMu.
	userCutoffs map[int64]uint

	// answeredThrough is the highest sequence a model turn's recorded reply
	// covered: every answerable user message at or below it has been
	// answered. Written under chatMemoriesMu; read under turnsMu too, where
	// a text flush checks it without taking the memory lock.
	answeredThrough atomic.Uint64

	// pinnedSeq is the media message that joined and that its turn has not
	// read yet (joinMedia, snapshotForMedia): the window's trim never drops
	// it. One at a time, as one turn holds the chat; 0 for none.
	pinnedSeq atomic.Uint64
}

// roleBusinessOwner is the UserRole of a message the business account itself
// sent in one of its Business chats (recordOwnerMessage): the business side of
// the conversation, as the bot's replies are, but written by a person.
const roleBusinessOwner = "business_owner"

const (
	ScopeStatsViewOwn        = "stats:view:own"
	ScopeStatsViewAny        = "stats:view:any"
	ScopeHistoryClearOwn     = "history:clear:own"
	ScopeHistoryClearAny     = "history:clear:any"
	ScopeHistoryClearHardOwn = "history:clear_hard:own"
	ScopeHistoryClearHardAny = "history:clear_hard:any"
	ScopeModelSet            = "model:set"
	ScopeUserPromote         = "user:promote"
	ScopeTTSUse              = "tts:use"
)

type Scope struct {
	gorm.Model
	Name string `gorm:"uniqueIndex"`
}

type Role struct {
	gorm.Model
	Name   string  `gorm:"uniqueIndex"`
	Scopes []Scope `gorm:"many2many:role_scopes;"`
}

type User struct {
	gorm.Model
	BotID      uint  `gorm:"uniqueIndex:idx_user_bot;index"`
	TelegramID int64 `gorm:"uniqueIndex:idx_user_bot;not null"`
	Username   string
	RoleID     uint
	Role       Role `gorm:"foreignKey:RoleID"`
	IsOwner    bool `gorm:"default:false"`
}

func (User) TableName() string {
	return "users"
}

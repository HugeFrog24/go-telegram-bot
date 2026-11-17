package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connectionsOf makes getBusinessConnection answer each connection in users
// as made by that user, enabled and with every right, and counts the calls.
func connectionsOf(tg *MockTelegramClient, users map[string]int64) *atomic.Int32 {
	var calls atomic.Int32
	tg.GetBusinessConnectionFunc = func(_ context.Context, p *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error) {
		calls.Add(1)
		id, _ := p.BusinessConnectionID.(string)
		return &models.BusinessConnection{ID: id, User: models.User{ID: users[id]},
			IsEnabled: true, Rights: fullBusinessRights()}, nil
	}
	return &calls
}

// connectionUpdate is a business_connection update for connection id of
// user, enabled or not.
func connectionUpdate(id string, user int64, enabled bool) *models.Update {
	return &models.Update{BusinessConnection: &models.BusinessConnection{ID: id, User: models.User{ID: user},
		IsEnabled: enabled, Rights: fullBusinessRights()}}
}

// captureInfo records InfoLogger for the rest of the test.
func captureInfo(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	prev := InfoLogger
	InfoLogger = log.New(&logged, "", 0)
	t.Cleanup(func() { InfoLogger = prev })
	return &logged
}

// B8: the bot serves a Business connection its owner made, looked up with
// getBusinessConnection when only the connection's id is known, and once.
func TestBusiness_TheOwnersConnectionIsServed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	tgMock := b.tgBot.(*MockTelegramClient)
	lookups := connectionsOf(tgMock, map[string]int64{"own-1": b.config.OwnerTelegramID})

	b.handleUpdate(context.Background(), nil, businessText("own-1", 1, customer, "hello shop"))
	b.handleUpdate(context.Background(), nil, businessText("own-1", 2, customer, "anyone there?"))

	require.Len(t, stub.requestBetas(), 2)
	require.Len(t, tg.sent(), 2)
	assert.Equal(t, "own-1", tg.sent()[0].BusinessConnectionID)
	assert.Equal(t, int32(1), lookups.Load(), "the connection's user is looked up once, then cached")
}

// B8: and one a user in business.allowed_connection_users made.
func TestBusiness_AnAllowlistedUsersConnectionIsServed(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	b.config.Business = &BusinessConfig{AllowedConnectionUsers: []int64{5555}}
	connectionsOf(b.tgBot.(*MockTelegramClient), map[string]int64{"shop-1": 5555})

	b.handleUpdate(context.Background(), nil, businessText("shop-1", 1, customer, "hello shop"))

	require.Len(t, stub.requestBetas(), 1)
	require.Len(t, tg.sent(), 1)
	assert.Equal(t, "shop-1", tg.sent()[0].BusinessConnectionID)
}

// B8: an update through a connection anyone else made is ignored: nothing is
// stored, nothing answered, no user is created, and one Info line names the
// connection and its user, however many updates come.
func TestBusiness_AStrangersConnectionIsIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	logged := captureInfo(t)
	connectionsOf(b.tgBot.(*MockTelegramClient), map[string]int64{"stranger-1": 9999})
	var usersBefore int64
	require.NoError(t, b.db.Model(&User{}).Count(&usersBefore).Error)

	b.handleUpdate(context.Background(), nil, businessText("stranger-1", 1, customer, "hello"))
	b.handleUpdate(context.Background(), nil, businessText("stranger-1", 2, customer, "/clear"))
	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 1, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: customer,
		BusinessConnectionID: "stranger-1", Text: "hello, edited"}})

	assert.Empty(t, stub.requestBetas(), "nothing is answered")
	assert.Empty(t, tg.sent(), "nothing is sent")
	var rows, users int64
	require.NoError(t, b.db.Model(&Message{}).Count(&rows).Error)
	require.NoError(t, b.db.Model(&User{}).Count(&users).Error)
	assert.Zero(t, rows, "nothing is stored")
	assert.Equal(t, usersBefore, users, "no user is created")
	assert.Equal(t, 1, strings.Count(logged.String(), "business connection stranger-1 of user 9999"), "logged once: %s", logged.String())
}

// B8/B10: a business_connection update fills the cache, so no lookup is
// needed; one that disables the connection drops it, and every update
// through it afterwards is ignored, without a lookup that could find it
// served; one that enables it again brings it back.
func TestBusiness_ADisabledConnectionIsDroppedAndIgnored(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, tg := historyTestBot(t, stub)
	lookups := connectionsOf(b.tgBot.(*MockTelegramClient), map[string]int64{"own-2": b.config.OwnerTelegramID})
	owner := b.config.OwnerTelegramID

	b.handleUpdate(context.Background(), nil, connectionUpdate("own-2", owner, true))
	b.handleUpdate(context.Background(), nil, businessText("own-2", 1, customer, "first"))
	require.Len(t, stub.requestBetas(), 1)

	b.handleUpdate(context.Background(), nil, connectionUpdate("own-2", owner, false))
	b.handleUpdate(context.Background(), nil, businessText("own-2", 2, customer, "after the disable"))
	assert.Len(t, stub.requestBetas(), 1, "a disabled connection is ignored")
	var after int64
	require.NoError(t, b.db.Model(&Message{}).Where("text = ?", "after the disable").Count(&after).Error)
	assert.Zero(t, after, "and nothing of it is stored")
	b.handleUpdate(context.Background(), nil, &models.Update{EditedBusinessMessage: &models.Message{
		ID: 1, Chat: models.Chat{ID: customer.ID, Type: models.ChatTypePrivate}, From: customer,
		BusinessConnectionID: "own-2", Text: "first, edited after the disable"}})
	assert.Contains(t, memoryIn(b, convKey{businessConnectionID: "own-2", chatID: customer.ID}), "first",
		"nor is an edit through it applied")

	b.handleUpdate(context.Background(), nil, connectionUpdate("own-2", owner, true))
	b.handleUpdate(context.Background(), nil, businessText("own-2", 3, customer, "back again"))
	assert.Len(t, stub.requestBetas(), 2, "re-enabled, it is served again")
	assert.Zero(t, lookups.Load(), "the updates kept the cache: no lookup was needed")
	assert.Len(t, tg.sent(), 2)
}

// B8: business.allowed_connection_users is read from a bot's config and
// validated: every entry a Telegram user ID (positive), none twice.
func TestConfig_BusinessAllowedConnectionUsers(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	dir := t.TempDir()
	path := dir + "/shop.json"
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"shop","telegram_token":"t","model":"m","active":true,
		"messages_per_hour":1,"messages_per_day":1,"business":{"allowed_connection_users":[5555,6666]}}`), 0o600))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Business)
	assert.Equal(t, []int64{5555, 6666}, cfg.Business.AllowedConnectionUsers)
	assert.NoError(t, validateConfig(&cfg, map[string]bool{}, map[string]bool{}))

	for name, tc := range map[string]struct {
		users []int64
		says  string
	}{
		"zero":     {[]int64{0}, "not a Telegram user ID"},
		"negative": {[]int64{5555, -100123}, "not a Telegram user ID"},
		"twice":    {[]int64{5555, 5555}, "more than once"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			bad.Business = &BusinessConfig{AllowedConnectionUsers: tc.users}
			err := validateConfig(&bad, map[string]bool{}, map[string]bool{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "business.allowed_connection_users")
			assert.Contains(t, err.Error(), tc.says)
		})
	}
	none := cfg
	none.Business = nil
	assert.NoError(t, validateConfig(&none, map[string]bool{}, map[string]bool{}), "the block is optional: no one but the owner")
}

// B8: a business_connection update that lands while a lookup of the same
// connection is in flight wins over the lookup's (older) answer.
func TestBusiness_AnUpdateDuringALookupWins(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	stub := newAnthropicStub(t, candleFarmStream())
	b, _ := historyTestBot(t, stub)
	owner := b.config.OwnerTelegramID
	tg := b.tgBot.(*MockTelegramClient)
	tg.GetBusinessConnectionFunc = func(context.Context, *bot.GetBusinessConnectionParams) (*models.BusinessConnection, error) {
		b.handleUpdate(context.Background(), nil, connectionUpdate("race-1", owner, false)) // the owner disconnects meanwhile
		return &models.BusinessConnection{ID: "race-1", User: models.User{ID: owner}, IsEnabled: true, Rights: fullBusinessRights()}, nil
	}

	b.handleUpdate(context.Background(), nil, businessText("race-1", 1, customer, "sent as it disconnected"))
	b.handleUpdate(context.Background(), nil, businessText("race-1", 2, customer, "after"))

	assert.Empty(t, stub.requestBetas(), "the disconnect is what the bot keeps")
}

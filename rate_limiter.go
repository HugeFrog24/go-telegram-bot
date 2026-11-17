package main

import (
	"context"
	"time"

	"golang.org/x/time/rate"
)

type userLimiter struct {
	hourlyLimiter   *rate.Limiter
	dailyLimiter    *rate.Limiter
	lastHourlyReset time.Time
	lastDailyReset  time.Time
	banUntil        time.Time
	clock           Clock
}

// Handled commands have a limit of their own per user, apart from the message
// limit: 5 at once, refilled at 5 per minute; 30 at once, refilled at 30 per
// hour. A refused command gets a notice at most once a minute.
const (
	commandsPerMinute = 5
	commandsPerHour   = 30
	commandsLimited   = "Too many commands, try again later."
)

type commandLimiter struct {
	perMinute, perHour, notice *rate.Limiter
}

// allowCommand takes one command from userID's limit, and reports whether it
// may run and, if not, whether to send the notice.
func (b *Bot) allowCommand(userID int64) (allowed, notify bool) {
	b.userLimitersMu.Lock()
	defer b.userLimitersMu.Unlock()
	if b.commandLimiters == nil {
		b.commandLimiters = make(map[int64]*commandLimiter)
	}
	limiter, exists := b.commandLimiters[userID]
	if !exists {
		limiter = &commandLimiter{
			perMinute: rate.NewLimiter(rate.Every(time.Minute/commandsPerMinute), commandsPerMinute),
			perHour:   rate.NewLimiter(rate.Every(time.Hour/commandsPerHour), commandsPerHour),
			notice:    rate.NewLimiter(rate.Every(time.Minute), 1),
		}
		b.commandLimiters[userID] = limiter
	}
	now := b.clock.Now()
	perMinute, perHour := limiter.perMinute.ReserveN(now, 1), limiter.perHour.ReserveN(now, 1)
	if perMinute.DelayFrom(now) == 0 && perHour.DelayFrom(now) == 0 {
		return true, false
	}
	perMinute.CancelAt(now)
	perHour.CancelAt(now)
	return false, limiter.notice.AllowN(now, 1)
}

// commandAllowed applies the command limit to user, from whom a handled
// command came; owners and admins are exempt. A refused command is told so,
// aside, at most once a minute.
func (b *Bot) commandAllowed(ctx context.Context, to replyTarget, user User) bool {
	if user.IsOwner || user.Role.Name == "admin" || user.Role.Name == "owner" {
		return true
	}
	allowed, notify := b.allowCommand(user.TelegramID)
	if allowed {
		return true
	}
	InfoLogger.Printf("[%s] command from user %d refused: over the command limit", b.config.ID, user.TelegramID)
	if notify {
		if err := b.sendResponse(ctx, to, commandsLimited); err != nil {
			ErrorLogger.Printf("Error sending the command limit notice: %v", err)
		}
	}
	return false
}

func (b *Bot) checkRateLimits(userID int64) bool {
	b.userLimitersMu.Lock()
	defer b.userLimitersMu.Unlock()

	limiter, exists := b.userLimiters[userID]
	if !exists {
		limiter = &userLimiter{
			hourlyLimiter:   rate.NewLimiter(rate.Every(time.Hour/time.Duration(b.config.MessagePerHour)), b.config.MessagePerHour),
			dailyLimiter:    rate.NewLimiter(rate.Every(24*time.Hour/time.Duration(b.config.MessagePerDay)), b.config.MessagePerDay),
			lastHourlyReset: b.clock.Now(),
			lastDailyReset:  b.clock.Now(),
			clock:           b.clock,
		}
		b.userLimiters[userID] = limiter
	}

	now := limiter.clock.Now()

	if now.Before(limiter.banUntil) {
		return false
	}

	if now.Sub(limiter.lastHourlyReset) >= time.Hour {
		limiter.hourlyLimiter = rate.NewLimiter(rate.Every(time.Hour/time.Duration(b.config.MessagePerHour)), b.config.MessagePerHour)
		limiter.lastHourlyReset = now
	}

	if now.Sub(limiter.lastDailyReset) >= 24*time.Hour {
		limiter.dailyLimiter = rate.NewLimiter(rate.Every(24*time.Hour/time.Duration(b.config.MessagePerDay)), b.config.MessagePerDay)
		limiter.lastDailyReset = now
	}

	dailyRes := limiter.dailyLimiter.ReserveN(now, 1)
	hourlyRes := limiter.hourlyLimiter.ReserveN(now, 1)
	if dailyRes.DelayFrom(now) > 0 || hourlyRes.DelayFrom(now) > 0 {
		dailyRes.CancelAt(now)
		hourlyRes.CancelAt(now)
		banDuration, err := time.ParseDuration(b.config.TempBanDuration)
		if err != nil {
			banDuration = 24 * time.Hour
		}
		limiter.banUntil = now.Add(banDuration)
		return false
	}

	return true
}

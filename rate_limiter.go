package main

import (
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

	// Check if the user is currently banned
	if now.Before(limiter.banUntil) {
		return false
	}

	// Reset hourly limiter if an hour has passed since the last reset
	if now.Sub(limiter.lastHourlyReset) >= time.Hour {
		limiter.hourlyLimiter = rate.NewLimiter(rate.Every(time.Hour/time.Duration(b.config.MessagePerHour)), b.config.MessagePerHour)
		limiter.lastHourlyReset = now
	}

	// Reset daily limiter if 24 hours have passed since the last reset
	if now.Sub(limiter.lastDailyReset) >= 24*time.Hour {
		limiter.dailyLimiter = rate.NewLimiter(rate.Every(24*time.Hour/time.Duration(b.config.MessagePerDay)), b.config.MessagePerDay)
		limiter.lastDailyReset = now
	}

	// Check if the message exceeds rate limits
	if !limiter.hourlyLimiter.Allow() || !limiter.dailyLimiter.Allow() {
		banDuration, err := time.ParseDuration(b.config.TempBanDuration)
		if err != nil {
			// If parsing fails, default to a 24-hour ban
			banDuration = 24 * time.Hour
		}
		limiter.banUntil = now.Add(banDuration)
		return false
	}

	return true
}

package main

import (
	"time"

	"golang.org/x/time/rate"
)

type userLimiter struct {
	hourlyLimiter *rate.Limiter
	dailyLimiter  *rate.Limiter
	lastReset     time.Time
	banUntil      time.Time
}

func (b *Bot) checkRateLimits(userID int64) bool {
	b.userLimitersMu.Lock()
	defer b.userLimitersMu.Unlock()

	limiter, exists := b.userLimiters[userID]
	if !exists {
		limiter = &userLimiter{
			hourlyLimiter: rate.NewLimiter(rate.Every(time.Hour/time.Duration(b.config.MessagePerHour)), b.config.MessagePerHour),
			dailyLimiter:  rate.NewLimiter(rate.Every(24*time.Hour/time.Duration(b.config.MessagePerDay)), b.config.MessagePerDay),
			lastReset:     time.Now(),
		}
		b.userLimiters[userID] = limiter
	}

	now := time.Now()

	if now.Before(limiter.banUntil) {
		return false
	}

	if now.Sub(limiter.lastReset) >= 24*time.Hour {
		limiter.dailyLimiter = rate.NewLimiter(rate.Every(24*time.Hour/time.Duration(b.config.MessagePerDay)), b.config.MessagePerDay)
		limiter.lastReset = now
	}

	if !limiter.hourlyLimiter.Allow() || !limiter.dailyLimiter.Allow() {
		banDuration, _ := time.ParseDuration(b.config.TempBanDuration)
		limiter.banUntil = now.Add(banDuration)
		return false
	}

	return true
}

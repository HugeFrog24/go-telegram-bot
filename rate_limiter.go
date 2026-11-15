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

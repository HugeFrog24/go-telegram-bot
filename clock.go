package main

import "time"

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time {
	return time.Now()
}

type MockClock struct {
	currentTime time.Time
}

func (mc *MockClock) Now() time.Time {
	return mc.currentTime
}

func (mc *MockClock) Advance(d time.Duration) {
	mc.currentTime = mc.currentTime.Add(d)
}

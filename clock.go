// clock.go
package main

import "time"

// Clock is an interface to abstract time-related functions.
type Clock interface {
	Now() time.Time
}

// RealClock implements Clock using the actual time.
type RealClock struct{}

// Now returns the current local time.
func (RealClock) Now() time.Time {
	return time.Now()
}

// MockClock implements Clock for testing purposes.
type MockClock struct {
	currentTime time.Time
}

// Now returns the mocked current time.
func (mc *MockClock) Now() time.Time {
	return mc.currentTime
}

// Advance moves the current time forward by the specified duration.
func (mc *MockClock) Advance(d time.Duration) {
	mc.currentTime = mc.currentTime.Add(d)
}

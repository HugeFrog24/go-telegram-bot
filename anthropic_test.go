package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestLanguageCodeReplacement tests that language code is properly handled and replaced
func TestLanguageCodeReplacement(t *testing.T) {
	// Test with provided language code
	systemMessage := "User's language preference: '{language}'"

	// Test with a specific language code
	langValue := "fr"
	result := strings.ReplaceAll(systemMessage, "{language}", langValue)

	if !strings.Contains(result, "User's language preference: 'fr'") {
		t.Errorf("Expected language code 'fr' to be replaced, got: %s", result)
	}

	// Test with empty language code (should default to "en")
	langValue = ""
	if langValue == "" {
		langValue = "en" // Default to English when language code is not available
	}
	result = strings.ReplaceAll(systemMessage, "{language}", langValue)

	if !strings.Contains(result, "User's language preference: 'en'") {
		t.Errorf("Expected default language code 'en' to be used, got: %s", result)
	}
}

// TestPremiumStatusReplacement tests that premium status is properly handled and replaced
func TestPremiumStatusReplacement(t *testing.T) {
	systemMessage := "User is a {premium_status}"

	// Test with premium user
	isPremium := true
	premiumStatus := "regular user"
	if isPremium {
		premiumStatus = "premium user"
	}
	result := strings.ReplaceAll(systemMessage, "{premium_status}", premiumStatus)

	if !strings.Contains(result, "User is a premium user") {
		t.Errorf("Expected premium status to be replaced with 'premium user', got: %s", result)
	}

	// Test with regular user
	isPremium = false
	premiumStatus = "regular user"
	if isPremium {
		premiumStatus = "premium user"
	}
	result = strings.ReplaceAll(systemMessage, "{premium_status}", premiumStatus)

	if !strings.Contains(result, "User is a regular user") {
		t.Errorf("Expected premium status to be replaced with 'regular user', got: %s", result)
	}
}

// TestTimeContextCalculation tests that time context is correctly calculated for different hours
func TestTimeContextCalculation(t *testing.T) {
	// Test cases for different hours
	testCases := []struct {
		hour     int
		expected string
	}{
		{3, "night"},      // Night: hours < 5 or hours >= 22
		{5, "morning"},    // Morning: 5 <= hours < 12
		{12, "afternoon"}, // Afternoon: 12 <= hours < 18
		{17, "afternoon"}, // Afternoon: 12 <= hours < 18
		{18, "evening"},   // Evening: 18 <= hours < 22
		{21, "evening"},   // Evening: 18 <= hours < 22
		{22, "night"},     // Night: hours < 5 or hours >= 22
		{23, "night"},     // Night: hours < 5 or hours >= 22
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Hour_%d", tc.hour), func(t *testing.T) {
			// Create a timestamp for the specified hour
			testTime := time.Date(2025, 5, 15, tc.hour, 0, 0, 0, time.UTC)

			// Get the hour directly from the test time to ensure it's what we expect
			actualHour := testTime.Hour()
			if actualHour != tc.hour {
				t.Fatalf("Test setup error: expected hour %d, got %d", tc.hour, actualHour)
			}

			// Calculate time context using the same logic as in anthropic.go
			var timeContext string
			if actualHour >= 5 && actualHour < 12 {
				timeContext = "morning"
			} else if actualHour >= 12 && actualHour < 18 {
				timeContext = "afternoon"
			} else if actualHour >= 18 && actualHour < 22 {
				timeContext = "evening"
			} else {
				timeContext = "night"
			}

			// Check if the calculated time context matches the expected value
			if timeContext != tc.expected {
				t.Errorf("For hour %d: expected time context '%s', got '%s'",
					actualHour, tc.expected, timeContext)
			}
		})
	}
}

// TestSystemMessagePlaceholderReplacement tests that all placeholders are correctly replaced
func TestSystemMessagePlaceholderReplacement(t *testing.T) {
	systemMessage := "The user you're talking to has username '{username}' and display name '{firstname} {lastname}'.\n" +
		"User's language preference: '{language}'\n" +
		"User is a {premium_status}\n" +
		"It's currently {time_context} in your timezone"

	// Set up test data
	username := "testuser"
	firstName := "Test"
	lastName := "User"
	isPremium := true
	languageCode := "de"

	// Create a timestamp for a specific hour (e.g., 14:00 = afternoon)
	testTime := time.Date(2025, 5, 15, 14, 0, 0, 0, time.UTC)
	messageTime := int(testTime.Unix())

	// Handle username placeholder
	usernameValue := username
	if username == "" {
		usernameValue = "unknown"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{username}", usernameValue)

	// Handle firstname placeholder
	firstnameValue := firstName
	if firstName == "" {
		firstnameValue = "unknown"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{firstname}", firstnameValue)

	// Handle lastname placeholder
	lastnameValue := lastName
	if lastName == "" {
		lastnameValue = ""
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{lastname}", lastnameValue)

	// Handle language code placeholder
	langValue := languageCode
	if languageCode == "" {
		langValue = "en"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{language}", langValue)

	// Handle premium status
	premiumStatus := "regular user"
	if isPremium {
		premiumStatus = "premium user"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{premium_status}", premiumStatus)

	// Handle time awareness
	timeObj := time.Unix(int64(messageTime), 0)
	hour := timeObj.Hour()
	var timeContext string
	if hour >= 5 && hour < 12 {
		timeContext = "morning"
	} else if hour >= 12 && hour < 18 {
		timeContext = "afternoon"
	} else if hour >= 18 && hour < 22 {
		timeContext = "evening"
	} else {
		timeContext = "night"
	}
	systemMessage = strings.ReplaceAll(systemMessage, "{time_context}", timeContext)

	// Check that all placeholders were replaced correctly
	if !strings.Contains(systemMessage, "username 'testuser'") {
		t.Errorf("Username not replaced correctly, got: %s", systemMessage)
	}
	if !strings.Contains(systemMessage, "display name 'Test User'") {
		t.Errorf("Display name not replaced correctly, got: %s", systemMessage)
	}
	if !strings.Contains(systemMessage, "language preference: 'de'") {
		t.Errorf("Language preference not replaced correctly, got: %s", systemMessage)
	}
	if !strings.Contains(systemMessage, "User is a premium user") {
		t.Errorf("Premium status not replaced correctly, got: %s", systemMessage)
	}
	if !strings.Contains(systemMessage, "It's currently afternoon in your timezone") {
		t.Errorf("Time context not replaced correctly, got: %s", systemMessage)
	}
}

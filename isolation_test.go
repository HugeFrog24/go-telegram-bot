package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test isolation from real APIs. TestMain calls isolateFromRealAPIs before any
// test runs, so no test can reach Anthropic, ElevenLabs or Telegram, however
// it builds its client and whatever credentials this machine holds.

// anthropicCredentialVars are every environment variable the Anthropic SDK
// reads a credential, a profile or a federation identity from.
var anthropicCredentialVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE",
	"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID",
	"ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_WORKSPACE_ID",
}

// blockedRequests counts requests the test transport refused because they
// were bound for another machine. TestMain fails the run when any were.
var blockedRequests atomic.Int64

// loopbackOnly is the test binary's http.DefaultTransport: it passes
// requests for this machine (the httptest stand-ins) to the real transport
// and refuses, counts and logs every other one.
type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if host := req.URL.Hostname(); host != "" && !isLoopbackHost(host) {
		blockedRequests.Add(1)
		fmt.Fprintln(os.Stderr, "test isolation: blocked a request to", req.URL.Host)
		return nil, fmt.Errorf("test isolation: request to %q blocked", req.URL.Host)
	}
	return l.next.RoundTrip(req)
}

// isolateFromRealAPIs points the Anthropic SDK at a loopback port nothing
// listens on and at an empty config directory (which it reads before
// %APPDATA%\Anthropic, $XDG_CONFIG_HOME or $HOME/.config), clears every
// credential variable, points ElevenLabs at the same port, and makes the
// standard transport refuse other hosts. The SDK's default HTTP client and
// http.DefaultClient (ElevenLabs, Telegram downloads) both go through it.
// It returns the cleanup for the config directory.
func isolateFromRealAPIs() (func(), error) {
	configDir, err := os.MkdirTemp("", "anthropic-config-")
	if err != nil {
		return nil, err
	}
	for _, key := range anthropicCredentialVars {
		if err := os.Unsetenv(key); err != nil {
			return nil, err
		}
	}
	for key, value := range map[string]string{
		"ANTHROPIC_BASE_URL":   "http://127.0.0.1:1",
		"ANTHROPIC_CONFIG_DIR": configDir,
	} {
		if err := os.Setenv(key, value); err != nil {
			return nil, err
		}
	}
	elevenLabsSTTURL, elevenLabsTTSURL = "http://127.0.0.1:1/stt", "http://127.0.0.1:1/tts/"
	http.DefaultTransport = loopbackOnly{next: http.DefaultTransport}
	return func() { _ = os.RemoveAll(configDir) }, nil
}

// isLoopbackHost reports whether host (no port) names this machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// plantProfile writes an `ant auth login` style profile into dir: a static
// token and a base URL that are not this machine's.
func plantProfile(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "configs"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "credentials"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "configs", "default.json"),
		[]byte(`{"version":"1.0","authentication":{"type":"user_oauth"},"base_url":"https://planted.invalid"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials", "default.json"),
		[]byte(`{"type":"oauth_token","access_token":"planted-token"}`), 0o600))
}

// unsetEnv removes key for the rest of the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	if old, ok := os.LookupEnv(key); ok {
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() { _ = os.Setenv(key, old) })
	}
}

// defaultClientTarget builds the client NewBot builds without an API key and
// reports where its first request would go and what credentials it carries.
// The request never leaves: the middleware stops it.
func defaultClientTarget(t *testing.T) (host, authorization, apiKey string) {
	t.Helper()
	client := anthropic.NewClient()
	_, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	},
		option.WithMaxRetries(0),
		option.WithMiddleware(func(req *http.Request, _ option.MiddlewareNext) (*http.Response, error) {
			host = req.URL.Hostname()
			authorization, apiKey = req.Header.Get("Authorization"), req.Header.Get("X-Api-Key")
			return nil, errors.New("stopped by the isolation guard")
		}))
	require.Error(t, err)
	return host, authorization, apiKey
}

// No test can reach the real Anthropic API, whatever this machine holds:
// TestMain points the SDK at a refusing loopback port and an empty config
// directory, and clears every credential variable. Profiles are planted
// where the SDK looks without ANTHROPIC_CONFIG_DIR (%APPDATA%\Anthropic on
// Windows, $XDG_CONFIG_HOME/anthropic or $HOME/.config/anthropic elsewhere),
// and the first subtest proves they would be read.
func TestIsolation_DefaultAnthropicClientStaysOnThisMachine(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	home := t.TempDir()
	for _, dir := range []string{
		filepath.Join(home, "Anthropic"),            // %APPDATA%\Anthropic
		filepath.Join(home, "anthropic"),            // $XDG_CONFIG_HOME/anthropic
		filepath.Join(home, ".config", "anthropic"), // $HOME/.config/anthropic
	} {
		plantProfile(t, dir)
	}
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)

	t.Run("the planted profile is what a machine without isolation would use", func(t *testing.T) {
		unsetEnv(t, "ANTHROPIC_CONFIG_DIR")
		unsetEnv(t, "ANTHROPIC_BASE_URL")
		host, authorization, _ := defaultClientTarget(t)
		assert.Equal(t, "planted.invalid", host)
		assert.Equal(t, "Bearer planted-token", authorization)
	})

	host, authorization, apiKey := defaultClientTarget(t)
	assert.True(t, isLoopbackHost(host), "the default client targets %q", host)
	assert.Empty(t, authorization, "no credential reaches a request")
	assert.Empty(t, apiKey, "no credential reaches a request")
}

// Every HTTP request a test makes through the standard transport, as the SDK,
// ElevenLabs and Telegram downloads do, must stay on this machine.
func TestIsolation_TransportRefusesOtherHosts(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	before := blockedRequests.Load()
	t.Cleanup(func() { blockedRequests.Store(before) }) // this test's own block is expected

	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test isolation")
	assert.Equal(t, before+1, blockedRequests.Load())
}

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestTimeContextFor(t *testing.T) {
	cases := []struct {
		hour     int
		expected string
	}{
		{3, "night"},
		{5, "morning"},
		{11, "morning"},
		{12, "afternoon"},
		{17, "afternoon"},
		{18, "evening"},
		{21, "evening"},
		{22, "night"},
		{23, "night"},
	}
	for _, tc := range cases {
		ts := int(time.Date(2025, 5, 15, tc.hour, 0, 0, 0, time.Local).Unix())
		if got := timeContextFor(ts); got != tc.expected {
			t.Errorf("timeContextFor(hour=%d) = %q, want %q", tc.hour, got, tc.expected)
		}
	}
}

func TestBuildUserContext(t *testing.T) {
	noon := int(time.Date(2025, 5, 15, 12, 0, 0, 0, time.Local).Unix())

	got := buildUserContext("alice", "Alice", "Smith", true, "de", noon)
	for _, want := range []string{"Alice Smith", "@alice", "Preferred language: de", "premium user", "afternoon"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildUserContext premium: missing %q in:\n%s", want, got)
		}
	}

	got = buildUserContext("", "", "", false, "", noon)
	for _, want := range []string{"User: unknown (Telegram @unknown)", "Preferred language: en", "regular user"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildUserContext fallback: missing %q in:\n%s", want, got)
		}
	}

	got = buildUserContext("bob", "Bob", "", false, "en", noon)
	if !strings.Contains(got, "User: Bob (Telegram @bob)") {
		t.Errorf("buildUserContext firstname-only: got:\n%s", got)
	}
}

func TestThinkingParamFromConfig(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		display string
		ok      bool
		want    map[string]any
	}{
		{"unset omits param", "", "", false, nil},
		{"unknown value omits param", "bogus", "", false, nil},
		{"adaptive no display", ThinkingModeAdaptive, "", true,
			map[string]any{"type": "adaptive"}},
		{"adaptive summarized", ThinkingModeAdaptive, ThinkingDisplaySummarized, true,
			map[string]any{"type": "adaptive", "display": "summarized"}},
		{"adaptive omitted", ThinkingModeAdaptive, ThinkingDisplayOmitted, true,
			map[string]any{"type": "adaptive", "display": "omitted"}},
		{"disabled", ThinkingModeDisabled, "", true,
			map[string]any{"type": "disabled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			union, ok := thinkingParamFromConfig(tc.mode, tc.display)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			raw, err := json.Marshal(union)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("wire shape %s: got %d keys, want %d (%v)", raw, len(got), len(tc.want), tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("wire shape %s: key %q = %v, want %v", raw, k, got[k], v)
				}
			}
		})
	}
}

func TestBackwardCompatibleParams(t *testing.T) {
	params := anthropic.BetaMessageNewParams{
		Model:     "claude-test",
		MaxTokens: defaultMaxTokens,
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
		},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["thinking"]; present {
		t.Errorf("zero Thinking union must omit the key; body: %s", raw)
	}
	if mt, ok := got["max_tokens"].(float64); !ok || int(mt) != defaultMaxTokens {
		t.Errorf("max_tokens = %v, want %d; body: %s", got["max_tokens"], defaultMaxTokens, raw)
	}
}

func TestWebSearchTools(t *testing.T) {
	t.Run("nil config yields no tools", func(t *testing.T) {
		if tools := webSearchTools(nil); tools != nil {
			t.Errorf("webSearchTools(nil) = %v, want nil", tools)
		}
	})

	t.Run("search only when fetch off", func(t *testing.T) {
		tools := webSearchTools(&WebSearchConfig{
			AllowedDomains: []string{"example.com/hc"},
			MaxUses:        3,
		})
		if len(tools) != 1 {
			t.Fatalf("got %d tools, want 1 (search only)", len(tools))
		}
		if tools[0].OfWebSearchTool20260318 == nil {
			t.Fatalf("tools[0] is not a web_search tool")
		}
		if tools[0].OfWebFetchTool20260318 != nil {
			t.Error("web_fetch tool present but fetch is off")
		}
	})

	t.Run("search + fetch with allowlist and citations", func(t *testing.T) {
		tools := webSearchTools(&WebSearchConfig{
			AllowedDomains:   []string{"example.com/hc", "docs.example.com"},
			MaxUses:          3,
			Fetch:            true,
			MaxContentTokens: 50000,
		})
		if len(tools) != 2 {
			t.Fatalf("got %d tools, want 2 (search + fetch)", len(tools))
		}

		search := tools[0].OfWebSearchTool20260318
		if search == nil {
			t.Fatalf("tools[0] is not a web_search tool")
		}
		if !sameStrings(search.AllowedDomains, []string{"example.com/hc", "docs.example.com"}) {
			t.Errorf("search AllowedDomains = %v, want the path-scoped list unchanged", search.AllowedDomains)
		}
		if search.MaxUses.Value != 3 {
			t.Errorf("search MaxUses = %d, want 3", search.MaxUses.Value)
		}

		fetch := tools[1].OfWebFetchTool20260318
		if fetch == nil {
			t.Fatalf("tools[1] is not a web_fetch tool")
		}
		if !sameStrings(fetch.AllowedDomains, []string{"example.com", "docs.example.com"}) {
			t.Errorf("fetch AllowedDomains = %v, want host-only [example.com docs.example.com]", fetch.AllowedDomains)
		}
		if fetch.MaxContentTokens.Value != 50000 {
			t.Errorf("fetch MaxContentTokens = %d, want 50000", fetch.MaxContentTokens.Value)
		}
		if fetch.Citations.Enabled.Value != true {
			t.Error("fetch citations not enabled")
		}
	})

	t.Run("fetch hosts are deduped", func(t *testing.T) {
		tools := webSearchTools(&WebSearchConfig{
			AllowedDomains: []string{"a.com/x", "a.com/y", "b.com"},
			Fetch:          true,
		})
		fetch := tools[1].OfWebFetchTool20260318
		if fetch == nil {
			t.Fatalf("tools[1] is not a web_fetch tool")
		}
		if !sameStrings(fetch.AllowedDomains, []string{"a.com", "b.com"}) {
			t.Errorf("fetch AllowedDomains = %v, want deduped [a.com b.com]", fetch.AllowedDomains)
		}
	})

	t.Run("social host is search-only via fetch_allowed_domains", func(t *testing.T) {
		tools := webSearchTools(&WebSearchConfig{
			AllowedDomains:      []string{"helpshift.example/hc", "x.com/thatskygame"},
			FetchAllowedDomains: []string{"helpshift.example"},
			Fetch:               true,
		})
		search := tools[0].OfWebSearchTool20260318
		if search == nil {
			t.Fatalf("tools[0] is not a web_search tool")
		}
		var searchHasSocial bool
		for _, d := range search.AllowedDomains {
			if d == "x.com/thatskygame" {
				searchHasSocial = true
			}
		}
		if !searchHasSocial {
			t.Errorf("search AllowedDomains = %v, want it to include x.com/thatskygame", search.AllowedDomains)
		}

		fetch := tools[1].OfWebFetchTool20260318
		if fetch == nil {
			t.Fatalf("tools[1] is not a web_fetch tool")
		}
		if !sameStrings(fetch.AllowedDomains, []string{"helpshift.example"}) {
			t.Errorf("fetch AllowedDomains = %v, want only [helpshift.example]", fetch.AllowedDomains)
		}
		for _, d := range fetch.AllowedDomains {
			if strings.HasPrefix(d, "x.com") {
				t.Errorf("fetch AllowedDomains leaked the social host: %v", fetch.AllowedDomains)
			}
		}
	})

	t.Run("wire shape carries allowed_domains", func(t *testing.T) {
		tools := webSearchTools(&WebSearchConfig{
			AllowedDomains: []string{"thatgamecompany.helpshift.com/hc"},
			MaxUses:        2,
			Fetch:          true,
		})
		raw, err := json.Marshal(tools)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body := string(raw)
		for _, want := range []string{
			"web_search_20260318",
			"web_fetch_20260318",
			"thatgamecompany.helpshift.com/hc",
			"allowed_domains",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("wire body missing %q:\n%s", want, body)
			}
		}
	})
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFetchHosts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil in nil out", nil, nil},
		{"empty in nil out", []string{}, nil},
		{"host passthrough", []string{"example.com"}, []string{"example.com"}},
		{"strip path", []string{"example.com/hc/en"}, []string{"example.com"}},
		{"dedup after strip", []string{"a.com/x", "a.com/y"}, []string{"a.com"}},
		{"preserve order and subdomains", []string{"docs.example.com/a", "example.com"}, []string{"docs.example.com", "example.com"}},
		{"drop empty leading slash", []string{"/oops", "ok.com"}, []string{"ok.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fetchHosts(tc.in); !sameStrings(got, tc.want) {
				t.Errorf("fetchHosts(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEmptyStreamError(t *testing.T) {
	err := emptyStreamError("max_tokens", 3900, 4000)
	for _, want := range []string{"output budget exhausted", "3900", "4000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("max_tokens case: %q missing %q", err.Error(), want)
		}
	}
	if got := emptyStreamError("end_turn", 0, 1000).Error(); got != "unexpected response format from Anthropic" {
		t.Errorf("generic case = %q", got)
	}
	if got := emptyStreamError("", 0, 1000).Error(); got != "unexpected response format from Anthropic" {
		t.Errorf("no-stop-reason case = %q", got)
	}
}

func TestDynamicFilteringEnabled(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		cfg  *WebSearchConfig
		want bool
	}{
		{"nil config defaults on", nil, true},
		{"unset defaults on", &WebSearchConfig{}, true},
		{"explicit true", &WebSearchConfig{DynamicFiltering: &on}, true},
		{"explicit false", &WebSearchConfig{DynamicFiltering: &off}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.DynamicFilteringEnabled(); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Models older than Claude 4.6 reject the 2026 web tools unless the tool is
// restricted to direct calls, so the opt-out must reach both tools.
func TestWebSearchTools_AllowedCallers(t *testing.T) { //NOSONAR go:S100 -- underscore separation is idiomatic in Go test names
	off := false

	tools := webSearchTools(&WebSearchConfig{Fetch: true})
	if got := tools[0].OfWebSearchTool20260318.AllowedCallers; got != nil {
		t.Errorf("default should omit allowed_callers, got %v", got)
	}
	if got := tools[1].OfWebFetchTool20260318.AllowedCallers; got != nil {
		t.Errorf("default should omit allowed_callers on fetch, got %v", got)
	}

	tools = webSearchTools(&WebSearchConfig{Fetch: true, DynamicFiltering: &off})
	if got := tools[0].OfWebSearchTool20260318.AllowedCallers; len(got) != 1 || got[0] != "direct" {
		t.Errorf("search allowed_callers = %v, want [direct]", got)
	}
	if got := tools[1].OfWebFetchTool20260318.AllowedCallers; len(got) != 1 || got[0] != "direct" {
		t.Errorf("fetch allowed_callers = %v, want [direct]", got)
	}
}

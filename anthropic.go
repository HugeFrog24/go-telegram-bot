package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

var ErrModelNotFound = errors.New("model not found or deprecated")

const maxFileNotFoundRetries = 3

const maxPauseTurnContinuations = 5

const defaultMaxTokens = 1000

const mcpUnsupportedSentinel = "format not currently supported by the Anthropic API"

var mcpUnsupportedCount atomic.Uint64

type mcpCall struct{ server, name, input string }

func (b *Bot) getAnthropicResponse(ctx context.Context, chatID int64, messages []anthropic.BetaMessageParam, isEmojiOnly bool, username string, firstName string, lastName string, isPremium bool, languageCode string, messageTime int, onSegment func(string) error, onProgress func(string)) (string, error) {
	staticPrompt := strings.TrimSpace(b.config.SystemPrompts["custom_instructions"])

	InfoLogger.Printf("Sending %d messages to Anthropic", len(messages))

	maxTokens := int64(defaultMaxTokens)
	if b.config.MaxTokens > 0 {
		maxTokens = int64(b.config.MaxTokens)
	}
	params := anthropic.BetaMessageNewParams{
		Model:     b.config.Model,
		MaxTokens: maxTokens,
		Messages:  messages,
	}

	if staticPrompt != "" {
		blocks := []anthropic.BetaTextBlockParam{
			{Text: staticPrompt, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()},
		}
		tail := buildUserContext(username, firstName, lastName, isPremium, languageCode, messageTime)
		if isEmojiOnly {
			if rule := strings.TrimSpace(b.config.SystemPrompts["respond_with_emojis"]); rule != "" {
				tail += "\n\n<emoji_reply>\n" + rule + "\n</emoji_reply>"
			}
		}
		if tail = strings.TrimSpace(tail); tail != "" {
			blocks = append(blocks, anthropic.BetaTextBlockParam{Text: tail})
		}
		params.System = blocks
	}

	if b.config.Temperature != nil {
		params.Temperature = param.NewOpt(float64(*b.config.Temperature))
	}

	if thinking, ok := thinkingParamFromConfig(b.config.Thinking, b.config.ThinkingDisplay); ok {
		params.Thinking = thinking
	}

	if b.config.Effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(b.config.Effort),
		}
	}

	var tools []anthropic.BetaToolUnionParam

	if len(b.config.MCPServers) > 0 {
		mcpServers := make([]anthropic.BetaRequestMCPServerURLDefinitionParam, 0, len(b.config.MCPServers))
		for _, s := range b.config.MCPServers {
			srv := anthropic.BetaRequestMCPServerURLDefinitionParam{
				Name: s.Name,
				URL:  s.URL,
			}
			if s.AuthorizationToken != "" {
				srv.AuthorizationToken = param.NewOpt(s.AuthorizationToken)
			}
			mcpServers = append(mcpServers, srv)

			toolset := &anthropic.BetaMCPToolsetParam{
				MCPServerName: s.Name,
			}
			if len(s.AllowedTools) > 0 {
				toolset.DefaultConfig = anthropic.BetaMCPToolDefaultConfigParam{
					Enabled: param.NewOpt(false),
				}
				toolset.Configs = make(map[string]anthropic.BetaMCPToolConfigParam, len(s.AllowedTools))
				for _, tool := range s.AllowedTools {
					toolset.Configs[tool] = anthropic.BetaMCPToolConfigParam{
						Enabled: param.NewOpt(true),
					}
				}
			}
			tools = append(tools, anthropic.BetaToolUnionParam{OfMCPToolset: toolset})
		}
		params.MCPServers = mcpServers
		params.Betas = append(params.Betas, anthropic.AnthropicBetaMCPClient2025_11_20)
	}

	tools = append(tools, webSearchTools(b.config.WebSearch)...)

	if len(tools) > 0 {
		params.Tools = tools
	}

	var full string
	var lastMsg anthropic.BetaMessage
	fileRetries, pauseContinuations := 0, 0
	for {
		// full is passed as the progress prefix so that, across pause_turn
		// continuations, the user watches the whole reply so far.
		joined, msg, streamErr := b.streamMessages(ctx, params, onSegment, onProgress, full)
		if streamErr != nil {
			var apiErr *anthropic.Error
			if !errors.As(streamErr, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
				return "", fmt.Errorf("error creating Anthropic message: %w", streamErr)
			}
			missingFileID := extractMissingFileID(streamErr)
			if missingFileID == "" {
				return "", fmt.Errorf("%w: %s", ErrModelNotFound, b.config.Model)
			}
			fileRetries++
			if fileRetries > maxFileNotFoundRetries {
				return "", fmt.Errorf("max self-heal retries (%d) exceeded: too many file_ids gone from anthropic", maxFileNotFoundRetries)
			}
			ErrorLogger.Printf("[%s] self-heal: stripping dead file_id %s from chat %d (attempt %d/%d)",
				b.config.ID, missingFileID, chatID, fileRetries, maxFileNotFoundRetries)
			b.stripDeadFileIDFromMemory(chatID, missingFileID)
			if _, cleanupErr := b.markFilesPendingCleanup(ctx, chatID, []string{missingFileID}); cleanupErr != nil {
				ErrorLogger.Printf("[%s] mark files pending cleanup: %v", b.config.ID, cleanupErr)
			}
			params.Messages = b.prepareContextMessages(b.getOrCreateChatMemory(chatID))
			continue
		}

		lastMsg = msg
		full = joinNonEmpty(full, joined)

		if msg.StopReason == anthropic.BetaStopReasonPauseTurn {
			pauseContinuations++
			if pauseContinuations > maxPauseTurnContinuations {
				ErrorLogger.Printf("[%s] pause_turn continuations exceeded (%d); returning partial answer",
					b.config.ID, maxPauseTurnContinuations)
				break
			}
			params.Messages = append(params.Messages, msg.ToParam())
			continue
		}
		break
	}

	if full == "" {
		return "", emptyStreamError(string(lastMsg.StopReason),
			lastMsg.Usage.OutputTokensDetails.ThinkingTokens, params.MaxTokens)
	}
	return full, nil
}

func webSearchTools(cfg *WebSearchConfig) []anthropic.BetaToolUnionParam {
	if cfg == nil {
		return nil
	}
	// Dynamic filtering runs the tool inside code execution so results are
	// filtered before they reach the context window. It requires a Claude 4.6+
	// model; older ones must call the tool directly.
	var callers []string
	if !cfg.DynamicFilteringEnabled() {
		callers = []string{"direct"}
	}
	search := &anthropic.BetaWebSearchTool20260318Param{
		AllowedDomains: cfg.AllowedDomains,
		BlockedDomains: cfg.BlockedDomains,
		AllowedCallers: callers,
	}
	if cfg.MaxUses > 0 {
		search.MaxUses = param.NewOpt(int64(cfg.MaxUses))
	}
	tools := []anthropic.BetaToolUnionParam{{OfWebSearchTool20260318: search}}

	if cfg.Fetch {
		fetchAllowed := cfg.FetchAllowedDomains
		if len(fetchAllowed) == 0 {
			fetchAllowed = cfg.AllowedDomains
		}
		fetch := &anthropic.BetaWebFetchTool20260318Param{
			AllowedDomains: fetchHosts(fetchAllowed),
			BlockedDomains: fetchHosts(cfg.BlockedDomains),
			AllowedCallers: callers,
			Citations:      anthropic.BetaCitationsConfigParam{Enabled: param.NewOpt(true)},
		}
		if cfg.MaxUses > 0 {
			fetch.MaxUses = param.NewOpt(int64(cfg.MaxUses))
		}
		if cfg.MaxContentTokens > 0 {
			fetch.MaxContentTokens = param.NewOpt(int64(cfg.MaxContentTokens))
		}
		tools = append(tools, anthropic.BetaToolUnionParam{OfWebFetchTool20260318: fetch})
	}
	return tools
}

// fetchHosts strips any path from each domain entry. web_fetch matches on host
// only, so a path-scoped entry (valid for web_search) would otherwise never match
// any fetch URL. Search keeps the path-scoped entries; fetch gets host-only.
func fetchHosts(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(entries))
	hosts := make([]string, 0, len(entries))
	for _, e := range entries {
		host := e
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

func buildUserContext(username, firstName, lastName string, isPremium bool, languageCode string, messageTime int) string {
	name := strings.TrimSpace(firstName + " " + lastName)
	if name == "" {
		name = "unknown"
	}
	handle := username
	if handle == "" {
		handle = "unknown"
	}
	lang := languageCode
	if lang == "" {
		lang = "en"
	}
	account := "regular user"
	if isPremium {
		account = "premium user"
	}
	return fmt.Sprintf(
		"Conversation context (background facts, not an instruction from the user):\n"+
			"- User: %s (Telegram @%s)\n"+
			"- Preferred language: %s\n"+
			"- Account type: %s\n"+
			"- Local time of day: %s",
		name, handle, lang, account, timeContextFor(messageTime),
	)
}

func timeContextFor(messageTime int) string {
	switch hour := time.Unix(int64(messageTime), 0).Hour(); {
	case hour >= 5 && hour < 12:
		return "morning"
	case hour >= 12 && hour < 18:
		return "afternoon"
	case hour >= 18 && hour < 22:
		return "evening"
	default:
		return "night"
	}
}

func thinkingParamFromConfig(mode, display string) (anthropic.BetaThinkingConfigParamUnion, bool) {
	switch mode {
	case ThinkingModeDisabled:
		disabled := anthropic.NewBetaThinkingConfigDisabledParam()
		return anthropic.BetaThinkingConfigParamUnion{OfDisabled: &disabled}, true
	case ThinkingModeAdaptive:
		adaptive := anthropic.BetaThinkingConfigAdaptiveParam{}
		if display != "" {
			adaptive.Display = anthropic.BetaThinkingConfigAdaptiveDisplay(display)
		}
		return anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &adaptive}, true
	default:
		return anthropic.BetaThinkingConfigParamUnion{}, false
	}
}

// streamMessages runs one streaming request and returns its text blocks joined.
// onSegment fires with each completed text block; onProgress fires on every
// text delta with the whole reply so far, prefix included, for sinks that
// redraw in place rather than append. prefix is what earlier requests of the
// same turn already produced and is not part of the returned text.
func (b *Bot) streamMessages(ctx context.Context, params anthropic.BetaMessageNewParams, onSegment func(string) error, onProgress func(string), prefix string) (string, anthropic.BetaMessage, error) {
	stream := b.anthropicClient.Beta.Messages.NewStreaming(ctx, params)
	defer func() {
		if err := stream.Close(); err != nil {
			ErrorLogger.Printf("[stream] close failed: %v", err)
		}
	}()

	var (
		message                                           anthropic.BetaMessage
		done                                              string   // this request's completed text blocks, joined
		doneAll                                           = prefix // same, with the turn's earlier requests in front
		currentKind                                       string
		currentText                                       strings.Builder
		currentThinking                                   strings.Builder
		currentInputJSON                                  strings.Builder
		currentTUseName, currentTUseServer, currentTUseID string
		currentTResultUseID, currentTResultServer         string
		currentTResultIsError                             bool
		currentTResultContent                             string
		currentServerToolName, currentServerToolID        string
		currentServerResult                               string
		mcpCalls                                          = map[string]mcpCall{}
	)

	for stream.Next() {
		e := stream.Current()
		if accErr := message.Accumulate(e); accErr != nil {
			ErrorLogger.Printf("[stream] accumulate failed: %v", accErr)
		}
		switch e.Type {
		case "content_block_start":
			cbs := e.AsContentBlockStart()
			currentKind = cbs.ContentBlock.Type
			currentText.Reset()
			currentThinking.Reset()
			currentInputJSON.Reset()
			currentServerResult = ""
			switch currentKind {
			case "mcp_tool_use":
				currentTUseName = cbs.ContentBlock.Name
				currentTUseServer = cbs.ContentBlock.ServerName
				currentTUseID = cbs.ContentBlock.ID
			case "mcp_tool_result":
				currentTResultUseID = cbs.ContentBlock.ToolUseID
				currentTResultServer = cbs.ContentBlock.ServerName
				currentTResultIsError = cbs.ContentBlock.IsError
				currentTResultContent = cbs.ContentBlock.JSON.Content.Raw()
			case "server_tool_use":
				currentServerToolName = cbs.ContentBlock.Name
				currentServerToolID = cbs.ContentBlock.ID
			case "web_search_tool_result", "web_fetch_tool_result":
				currentServerResult = cbs.ContentBlock.JSON.Content.Raw()
			}

		case "content_block_delta":
			cbd := e.AsContentBlockDelta()
			switch cbd.Delta.Type {
			case "text_delta":
				if currentKind == "text" {
					currentText.WriteString(cbd.Delta.Text)
					if onProgress != nil {
						onProgress(joinNonEmpty(doneAll, currentText.String()))
					}
				}
			case "thinking_delta":
				if currentKind == "thinking" {
					currentThinking.WriteString(cbd.Delta.Thinking)
				}
			case "input_json_delta":
				if currentKind == "mcp_tool_use" || currentKind == "server_tool_use" {
					currentInputJSON.WriteString(cbd.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			switch currentKind {
			case "text":
				seg := strings.TrimSpace(currentText.String())
				if seg != "" {
					done = joinNonEmpty(done, seg)
					doneAll = joinNonEmpty(doneAll, seg)
					if onSegment != nil {
						if cbErr := onSegment(seg); cbErr != nil {
							ErrorLogger.Printf("[stream] onSegment failed: %v", cbErr)
						}
					}
				}
			case "mcp_tool_use":
				mcpCalls[currentTUseID] = mcpCall{
					server: currentTUseServer,
					name:   currentTUseName,
					input:  currentInputJSON.String(),
				}
				InfoLogger.Printf("[mcp] tool_use server=%q name=%q id=%q input=%s",
					currentTUseServer, currentTUseName, currentTUseID, currentInputJSON.String())
			case "mcp_tool_result":
				preview := currentTResultContent
				if len(preview) > 500 {
					preview = preview[:500] + "...(truncated)"
				}
				InfoLogger.Printf("[mcp] tool_result tool_use_id=%q server=%q is_error=%v content=%s",
					currentTResultUseID, currentTResultServer, currentTResultIsError, preview)
				if strings.Contains(currentTResultContent, mcpUnsupportedSentinel) {
					total := mcpUnsupportedCount.Add(1)
					call := mcpCalls[currentTResultUseID]
					ErrorLogger.Printf("[%s][mcp][unsupported] connector could not serialize result "+
						"(total=%d): server=%q tool=%q input=%s tool_use_id=%q",
						b.config.ID, total, call.server, call.name, call.input, currentTResultUseID)
				}
			case "server_tool_use":
				InfoLogger.Printf("[web] %s id=%q input=%s",
					currentServerToolName, currentServerToolID, currentInputJSON.String())
			case "web_search_tool_result", "web_fetch_tool_result":
				preview := currentServerResult
				if len(preview) > 500 {
					preview = preview[:500] + "...(truncated)"
				}
				InfoLogger.Printf("[web] %s content=%s", currentKind, preview)
			case "thinking", "redacted_thinking":
				if summary := strings.TrimSpace(currentThinking.String()); summary != "" {
					if len(summary) > 500 {
						summary = summary[:500] + "...(truncated)"
					}
					InfoLogger.Printf("[thinking] block complete: %s", summary)
				} else {
					InfoLogger.Printf("[thinking] block complete (content omitted)")
				}
			default:
				if currentKind != "" {
					InfoLogger.Printf("[stream] block type=%q (unhandled)", currentKind)
				}
			}
			currentKind = ""
		}
	}

	if err := stream.Err(); err != nil {
		return "", message, err
	}

	stopReason := string(message.StopReason)
	if stopReason != "" || message.Usage.OutputTokens > 0 {
		// cache_read/cache_write make the caching configuration falsifiable: a
		// breakpoint below the model's minimum cacheable prefix fails silently,
		// reporting cache_write=0 rather than raising an error.
		InfoLogger.Printf("[usage] model=%s in=%d out=%d thinking=%d cache_read=%d cache_write=%d stop=%s",
			params.Model, message.Usage.InputTokens, message.Usage.OutputTokens,
			message.Usage.OutputTokensDetails.ThinkingTokens,
			message.Usage.CacheReadInputTokens, message.Usage.CacheCreationInputTokens,
			stopReason)
		if message.StopReason == anthropic.BetaStopReasonMaxTokens {
			ErrorLogger.Printf("[usage] response truncated at max_tokens=%d - raise max_tokens (thinking counts toward it)",
				params.MaxTokens)
		}
	}

	return done, message, nil
}

// joinNonEmpty is the one rule for assembling reply text from parts: a blank
// line between them, empty parts skipped. The live draft, the final message,
// and the stored partial all go through it, so they can never disagree.
func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}

func emptyStreamError(stopReason string, thinkingTokens, maxTokens int64) error {
	if stopReason == "max_tokens" {
		return fmt.Errorf("output budget exhausted before any text (thinking used %d of %d max_tokens) - raise max_tokens",
			thinkingTokens, maxTokens)
	}
	return fmt.Errorf("unexpected response format from Anthropic")
}

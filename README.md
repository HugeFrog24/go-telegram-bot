# Go Telegram Multibot

A scalable, multi-bot solution for Telegram using Go, GORM, and the Anthropic API.

## Design Considerations

- AI-powered (Anthropic Claude)
- Voice message support (ElevenLabs STT + TTS) — optional, enabled per bot via config
- Supports multiple bot profiles
- Uses SQLite for persistence
- Implements rate limiting and user management
- Modular architecture
- Comprehensive unit tests

## Usage

### Docker Deployment (Recommended)

1. Clone the repository:

   ```bash
   git clone https://github.com/HugeFrog24/go-telegram-bot.git
   cd go-telegram-bot
   ```

2. Copy the default config template and edit it:
   ```bash
   cp config/default.json config/mybot.json
   nano config/mybot.json
   ```

> [!IMPORTANT]
> Keep your config files secret and do not commit them to version control.

3. Create data directory and run:
   ```bash
   mkdir -p data
   docker-compose up -d
   ```

### Native Deployment

1. Install using `go get`:

   ```bash
   go get -u github.com/HugeFrog24/go-telegram-bot
   cd go-telegram-bot
   ```

2. Configure as above, then build:
   ```bash
   go build -o telegram-bot
   ```

## Trying Out New Behavior Safely

Want to experiment with a different personality, tone, or set of instructions without disturbing the bot your users already talk to? Run a second, separate bot just for testing.

Each bot profile is its own config file with its own Telegram token, and bots are fully independent — separate identity, separate chat history, separate settings. So a "test twin" is quick to set up:

1. Create a new bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Copy your existing config to a new file, e.g. `cp config/mybot.json config/mybot-test.json`.
3. In the new file, paste the new token, give it a different `id`, and edit `system_prompts` to try your changes.
4. Start it alongside your main bot. Chat with the test bot, tweak its prompt, and restart the test bot to try again — your real users never see the experiments.
5. Happy with the result? Copy the same change into your main bot's config and restart it.

> [!NOTE]
> A test bot always needs its **own** token. Telegram only lets one running bot listen on a given token, so you can't point a second copy at your live bot — give the twin its own @BotFather bot instead.

## Configuration

Each bot is one JSON file in `config/` (see `config/default.json` for the template). Keys of note:

| Key                | Type   | Default     | Description |
| ------------------ | ------ | ----------- | ----------- |
| `max_tokens`       | number | `1000`      | Maximum output tokens per reply. **Thinking tokens count toward this limit** — raise it (e.g. `4000`+) whenever `thinking` is `"adaptive"`, or a turn can spend the whole budget on reasoning and produce no text. |
| `thinking`         | string | *(omitted)* | Reasoning mode: `"adaptive"` (the model decides when and how much to think) or `"disabled"`. Omit the key entirely to use the model's own API default. If the configured model doesn't support the chosen mode, the API rejects the request with a 400 — owners/admins see the raw error, regular users get the generic fallback. Check [Anthropic's model docs](https://platform.claude.com/docs/en/build-with-claude/adaptive-thinking) for per-model support. |
| `thinking_display` | string | *(omitted)* | `"summarized"` or `"omitted"`. Only valid together with `"thinking": "adaptive"`. Controls whether the API returns a readable summary of the reasoning (logged, never sent to chat). Thinking is billed the same either way; when omitted, the API's per-model default applies. |
| `effort`           | string | *(omitted)* | `"low"`, `"medium"`, `"high"`, `"xhigh"` or `"max"`. Caps how much reasoning and token spend a turn may use; the API default is `high`. For short-reply chat bots `"low"` or `"medium"` cuts cost noticeably. Rejected with a 400 on models older than Claude 4.6 (Haiku 4.5, Sonnet 4.5) — omit it there. |
| `debounce_ms`      | number | *(omitted)* | Quiet window, in milliseconds, for coalescing rapid follow-up messages into a single turn. Omit or set `0` to disable. See [Coalescing rapid messages](#coalescing-rapid-messages) below. |
| `stream_drafts`    | bool   | `false`     | Streams each reply into one Telegram draft that grows in place, with a native Stop button, and sends it as a single message when done. Private chats only. See [Draft streaming and the Stop button](#draft-streaming-and-the-stop-button) below. |
| `cache_history`    | bool   | `true`      | Places a prompt-cache breakpoint on the trailing conversation block, so each turn reads the prior history from cache instead of reprocessing it at full price. Set `false` to cache only the system prompt. |
| `web_search`       | object | *(omitted)* | Enables Anthropic's server-side web search (and, optionally, web fetch), sandboxed to a domain allowlist. Omit the key entirely to leave both tools off — an absent block sends byte-identical requests to before. See [Web search / fetch](#web-search--fetch) below. |

Every reply logs one accounting line — `[usage] model=... in=... out=... thinking=... cache_read=... cache_write=... stop=...` — so thinking spend (billed even when its text is omitted) stays visible in `journalctl`/`docker compose logs`. A `stop=max_tokens` line is accompanied by an error-level warning that the reply was truncated.

> [!TIP]
> Watch `cache_read`/`cache_write` after changing prompts or models. A cache breakpoint below the model's minimum cacheable prefix fails **silently** — no error, just `cache_write=0` forever. The minimum is model-specific and not monotonic across generations (Haiku 4.5 needs 4096 tokens; Sonnet 4.6 needs 1024), so a short system prompt that caches fine on one model may never cache on another. Note also that chat memory is a sliding window: once it is full, each turn evicts the oldest message and changes the prefix, so `cache_read` on long-running chats will be lower than on fresh ones.

### Coalescing rapid messages

A user who sends "how do I do X", then "sorry typo", then "lmao" in five seconds would otherwise get three separate replies — the bot starts a full turn per message, because each Telegram update independently drives one. `debounce_ms` holds text messages in a per-chat buffer and resets the window on every new message, dispatching a single turn once the user stops typing.

```json
"debounce_ms": 2500
```

Reasonable windows are 1500–3000ms for ordinary chat and up to 8000ms for Telegram Business, where a person writing to a business account tends to send longer bursts. The maximum is 30000ms.

Nothing is discarded while buffering. Each message is still persisted and added to chat memory the moment it arrives, so the single coalesced turn sees all of them — the buffer only decides *when* to answer, never *what* the model reads. Reply metadata (language, premium status, business connection) follows the most recent message in the batch.

What deliberately does **not** wait:

- **Commands** (`/stats`, `/clear`, …) dispatch immediately.
- **Photos, albums, voice, and stickers** dispatch immediately and cancel any pending text window. The buffered text is not lost — it is already in memory, so the media turn answers it too.
- **`/clear` and `/clear_hard`** cancel the buffer outright. Without this, the window would fire seconds after the wipe and replay the just-deleted messages back into memory.

**Edits count as typing.** Mobile clients deliver autocorrect fixes as `edited_message` updates a few seconds after the send. The stored text (or photo caption) is replaced with the edit. If the message is still inside its quiet window, the window restarts. If a turn is already reading the old wording and has shown nothing yet, that turn restarts with the correction. An edit to a message that was already answered only corrects the record.

#### Messages that arrive mid-turn

No window catches every slow typist, and no LLM API lets you append to a request already in flight, so there is also a rule for a message that lands *after* the window closed, while the model is still working. At most one turn runs per chat, and a text flush that finds one running is handled in one of three ways:

- **Dropped** if the running turn's context already includes the message. Its reply answers the new message too.
- **Restarted** if nothing has been shown to the user yet. The running request is cancelled and rerun with the new message included. This is the only case that wastes tokens, and it is bounded by the time to first output; with `cache_history` on, the rerun reads the conversation prefix from cache.
- **Collected** if output is already on screen. The message gets its own turn as soon as the running one finishes, instead of a second concurrent one. If the finished turn's context turns out to have included the message after all, the extra turn is skipped.

Photos, voice, and stickers wait for a running turn to finish and are never restarted. Photo uploads still start immediately and overlap the running turn; only the model call queues behind it.

While a turn is running the bot shows Telegram's "typing…" indicator (or "recording audio" while synthesising a voice reply), refreshed every 4 seconds because Telegram expires the status after 5. Without it, a debounce window reads as the bot ignoring you — which is what prompts users to send more messages in the first place.

> [!NOTE]
> A cancelled request is not free. Anthropic bills the input tokens and any output generated before the connection closed, and a web search the turn already ran is billed again on the rerun. That is why a restart only happens before anything was shown, and why a message that never dispatched is still the cheapest message of all.

### Draft streaming and the Stop button

```json
"stream_drafts": true
```

With `stream_drafts` on, a reply in a private chat is streamed into one Telegram draft that grows in place (`sendMessageDraft`, available to every bot since Bot API 9.5), then sent as a single real message when the model finishes. Long replies split at 4096 characters. The draft is updated at most once per second, matching Telegram's per-chat send guidance, and re-sent every 10 seconds through silent stretches such as a web search so the preview does not expire.

The draft carries Telegram's native **Stop** button (Bot API 10.3). Tapping it cancels the Anthropic stream; whatever the user had watched being written is sent as a normal message and stored in memory, so the next turn continues from the reply the user actually saw. A message the user sent before tapping Stop still gets its own turn afterwards. The draft text is capped at 4096 characters; the final message is split instead.

What falls back to the previous one-message-per-text-block delivery:

- **Business connections and group chats.** Telegram documents drafts for private chats only, and `sendMessageDraft` has no `business_connection_id` parameter.
- **A rejected draft call.** After the first error the bot stops touching the draft and just sends the final message.

> [!IMPORTANT]
> While a draft is on screen, Telegram Android disables the composer: the user cannot send text or commands until the final message lands, and Telegram has said this is intended. The Stop button is their way out. For that reason the draft only appears once the model starts producing text; tool calls before that are covered by the typing indicator, which locks nothing. A March 2026 report described drafts rendering under "Pinned Messages" on iOS, so check the behaviour on your own client before enabling this for users.

### Web search / fetch

The `web_search` block wires in Anthropic's server-side `web_search` (and optionally `web_fetch`) tools, mirroring how `mcp_servers` wires in MCP toolsets: the engine provides the generic mechanism, and the per-bot config carries the policy. **No domains are hardcoded in the engine** — each profile declares its own allowlist. When the model decides a question needs current information, it runs a search server-side; results and any fetch targets are confined to the domains you list.

```json
"web_search": {
  "allowed_domains": [
    "example.com/help",
    "docs.example.com"
  ],
  "max_uses": 3,
  "fetch": true,
  "max_content_tokens": 50000
}
```

| Field                  | Type     | Description |
| ---------------------- | -------- | ----------- |
| `allowed_domains`      | string[] | Restricts **`web_search`** results (and, by default, `web_fetch` targets) to these. Each entry is a **domain with an optional path prefix and no scheme** — `example.com` or `example.com/help`. Mutually exclusive with `blocked_domains`. **The path only scopes `web_search`** — see the asymmetry note below. |
| `blocked_domains`      | string[] | Excludes these domains instead of allowlisting. Mutually exclusive with `allowed_domains` (setting both fails validation at boot, since the API 400s). |
| `fetch_allowed_domains`| string[] | **Host-only** allowlist for `web_fetch`, independent of the search list. Omit it and `web_fetch` reuses the *hosts* of `allowed_domains`. Set it to fetch a **narrower** set than you search — e.g. to keep a shared host (a social platform) search-only. Only meaningful with `fetch: true`. |
| `max_uses`             | number   | Caps how many searches the model may run per turn. Omit for no cap. |
| `fetch`                | bool     | Also enable `web_fetch`, which pulls full page content for URLs already surfaced by a search or pasted by the user (it cannot fetch model-invented URLs). Citations are always on when fetch is enabled, so fetched passages are sourceable. |
| `max_content_tokens`   | number   | Caps the tokens a single `web_fetch` may pull into context. Only meaningful with `fetch: true`. |
| `dynamic_filtering`    | bool     | Defaults to `true`. Lets the tools filter results inside Anthropic's sandbox before they enter context — see below. Set `false` for models older than Claude 4.6, which reject the tool unless it is called directly. |

**Search/fetch path asymmetry (important).** Anthropic filters the two tools differently: `web_search` honors a path prefix (`example.com/help` matches only `example.com/help/...`), but **`web_fetch` matches on the host only — an entry that includes a path never matches any fetch URL**. The engine bridges this: `web_search` gets your entries verbatim, while `web_fetch` gets the host portion of each entry (path stripped, deduped). So a path-scoped `allowed_domains` still permits fetching across the whole host. If that host isn't wholly trusted, list the fetchable hosts explicitly in `fetch_allowed_domains` instead.

**Search-only pattern for shared hosts (e.g. social).** To let the model *search* a single account on a shared platform without ever *fetching* the wider platform, put the account path in `allowed_domains` and leave its host out of `fetch_allowed_domains`:

```json
"web_search": {
  "allowed_domains": ["helpcenter.example/hc", "x.com/youraccount"],
  "fetch_allowed_domains": ["helpcenter.example"],
  "fetch": true
}
```

Here search is confined to your help center *and* your one social account, but fetch can only ever reach `helpcenter.example` — a pasted `x.com/someone-else` URL is not fetchable. Note the search side only sandboxes cleanly on platforms that keep an account's content under its handle path (X/Twitter, TikTok `@handle`, Facebook); use the `handle/` or `handle/*` form to avoid a prefix bleed. Instagram is an exception — posts live at `instagram.com/p/<code>`, not under the handle — so it can't be account-sandboxed by path.

The allowlist is a **hard, server-side sandbox**, not a prompt request — off-list results are dropped and an off-list fetch target returns `url_not_allowed`, which is the exfiltration mitigation Anthropic recommends for bots processing untrusted input. Keep the list tight. A fetch can still fail for site-side reasons (bot protection such as Cloudflare returns `url_not_accessible`); the search snippet plus citations remain the reliable signal, so a full fetch is a bonus, not a dependency.

`web_fetch` can only open a URL that a `web_search` result surfaced (or the user pasted) — never one the model invents or rebuilds from a page title; those return `url_not_in_prior_context`. So a fetch-enabled bot's prompt should steer it to **search first, then fetch a returned URL**, and to **re-search with a more specific query** (rather than guess a URL) when the exact page it wants isn't in the results.

**Dynamic filtering.** The engine uses the `web_search_20260318` / `web_fetch_20260318` tools, which run the search inside Anthropic's code-execution sandbox and let the model discard irrelevant parts of a result before it reaches the context window. On token-heavy sources (a long docs page, a GitHub README) this is the difference between paying for the whole page and paying for the part that answered the question; there is no extra charge for the code execution it uses. It needs a **Claude 4.6 or later** model — on anything older (Haiku 4.5, Sonnet 4.5) the API rejects the request unless the tool is called directly, so set `"dynamic_filtering": false` on those profiles. Disabling it also restores Zero Data Retention eligibility, which the sandbox otherwise forfeits.

Web search is **not free** — Anthropic bills per search (plus the tokens the results add) — so scope the allowlist and `max_uses` deliberately, and lean on a prompt that tells the bot to search only when a question actually turns on current or authoritative information. Search activity logs as `[web] ...` lines, and long multi-search turns are handled transparently (the engine follows Anthropic's `pause_turn` continuations up to a small cap, so a slow search doesn't truncate the reply).

> [!TIP]
> For deep request/response debugging, the Anthropic Go SDK ships `option.WithDebugLog(...)` (dumps full HTTP bodies with auth headers redacted). It is not wired into the bot — dev-only, add it temporarily to the client constructor if you ever need wire-level traces.

### Future: persistent memory

The Anthropic memory tool (`memory_20250818`) is a candidate future feature for cross-conversation recall (a self-hosted analog of ChatGPT's "memory"). The Go SDK already ships the types (`BetaMemoryTool20250818Param` and its tool-union slot plus the six-command union: `view`/`create`/`str_replace`/`insert`/`delete`/`rename`), but — unlike the Python/TypeScript/Java SDKs — provides **no handler helper**: the bot would have to hand-write client-side command dispatch against per-chat storage, including strict path validation (canonicalize and confine every model-supplied path under a fixed memory root; reject `..`/symlink traversal) and a no-secrets policy for stored content. Not implemented yet.

## Systemd Unit Setup

To enable the bot to start automatically on system boot and run in the background, set up a systemd unit.

1. Copy the systemd unit template and edit it:

   ```bash
   sudo cp examples/systemd/telegram-bot.service /etc/systemd/system/telegram-bot.service
   ```

   Edit the service file:

   ```bash
   sudo nano /etc/systemd/system/telegram-bot.service
   ```

   Adjust the following parameters:

   - WorkingDirectory
   - ExecStart
   - User

2. Enable and start the service:

   ```bash
   sudo systemctl daemon-reload
   ```

   ```bash
   sudo systemctl enable telegram-bot
   ```

   ```bash
   sudo systemctl start telegram-bot
   ```

3. Check the status:

   ```bash
   sudo systemctl status telegram-bot
   ```

For more details on the systemd setup, refer to the [demo service file](examples/systemd/telegram-bot.service).

## Logs

### Docker

```bash
docker-compose logs -f telegram-bot
```

### Systemd

```bash
journalctl -u telegram-bot -f
```

## Commands

| Command                           | Access      | Description                                                  |
| --------------------------------- | ----------- | ------------------------------------------------------------ |
| `/stats`                          | All users   | Show global bot statistics (total users and messages)        |
| `/stats user`                     | All users   | Show your own message statistics                             |
| `/stats user <user_id>`           | Admin/Owner | Show statistics for a specific user                          |
| `/whoami`                         | All users   | Show your Telegram ID, username, and role                    |
| `/clear`                          | All users   | Soft-delete your own chat history                            |
| `/clear <user_id>`                | Admin/Owner | Soft-delete all messages for a user across every chat        |
| `/clear <user_id> <chat_id>`      | Admin/Owner | Soft-delete a user's messages in a specific chat             |
| `/clear_hard`                     | All users   | Permanently delete your own chat history                     |
| `/clear_hard <user_id>`           | Admin/Owner | Permanently delete all messages for a user across every chat |
| `/clear_hard <user_id> <chat_id>` | Admin/Owner | Permanently delete a user's messages in a specific chat      |
| `/set_model <model-id>`           | Admin/Owner | Switch the AI model live without restarting                  |

> **Note:** In private DMs each user's `chat_id` equals their `user_id`. The scoped `<chat_id>` form is mainly useful for group chat moderation.

## Testing

The GitHub actions workflow already runs tests on every commit:

> [![CI](https://github.com/HugeFrog24/go-telegram-bot/actions/workflows/go-ci.yaml/badge.svg?branch=main)](https://github.com/HugeFrog24/go-telegram-bot/actions/workflows/go-ci.yaml)

However, you can run the tests locally using:

```bash
go test -race -v ./...
```

## Storage

At the moment, a SQLite database (`./data/bot.db`) is used for persistent storage.

Remember to back it up regularly.

Future versions will support more robust storage backends.

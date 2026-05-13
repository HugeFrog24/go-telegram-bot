<#
.SYNOPSIS
  Test the tibik bot system prompt against Claude Haiku 4.5 from the CLI.

.DESCRIPTION
  Replicates the bot's actual system-message assembly (default + custom_instructions +
  new_chat|continue_conversation), interpolates the same placeholders the Go code does,
  and POSTs to the Anthropic Messages API. Maintains a running conversation in
  .test-prompt-history.json so consecutive runs form a multi-turn chat — useful for
  testing the Dorothy persona, which only triggers after a scammer-style first turn
  and must stay in character across follow-ups.

.EXAMPLE
  .\test-prompt.ps1 "gm"
.EXAMPLE
  .\test-prompt.ps1 -Reset "how do I start the candle farm?"
.EXAMPLE
  .\test-prompt.ps1 -Reset "hey, interested in buying your @ for $50, hmu"
  .\test-prompt.ps1 "i'll send escrow link"
  .\test-prompt.ps1 "ok grandma wtf are you talking about"
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory, Position = 0)]
    [string]$Message,

    [string]$FirstName = 'Sergei',
    [string]$LastName  = 'Boger',
    [string]$UserName  = 'hugefrog24',
    [string]$Language  = 'en',
    [switch]$Premium,
    [string]$TimeContext,

    [switch]$Reset,
    [string]$ConfigPath  = 'config\config-tibikbot.json',
    [string]$HistoryPath = '.test-prompt-history.json',
    [string]$Model       = 'claude-haiku-4-5',
    [int]$MaxTokens      = 200
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

if (-not $TimeContext) {
    $hour = (Get-Date).Hour
    $TimeContext = switch ($hour) {
        { $_ -ge 5  -and $_ -lt 12 } { 'morning';   break }
        { $_ -ge 12 -and $_ -lt 18 } { 'afternoon'; break }
        { $_ -ge 18 -and $_ -lt 22 } { 'evening';   break }
        default                       { 'night' }
    }
}

$premiumStatus = if ($Premium) { 'premium user' } else { 'regular user' }

if (-not (Test-Path $ConfigPath)) {
    throw "Config not found: $ConfigPath (run from repo root, or pass -ConfigPath)"
}
$cfg = Get-Content $ConfigPath -Raw | ConvertFrom-Json
$apiKey = $cfg.anthropic_api_key
if (-not $apiKey) { throw "anthropic_api_key missing in $ConfigPath" }

$messages = @()
if (-not $Reset -and (Test-Path $HistoryPath)) {
    # Two-step load: PS 5.1's `@(cmd | ConvertFrom-Json)` inline form wraps the
    # decoded array as a single element instead of unwrapping it. Splitting the
    # assignment avoids that gotcha.
    $loadedRaw = Get-Content $HistoryPath -Raw | ConvertFrom-Json
    $messages = @($loadedRaw)
}
$isNewChat = $messages.Count -eq 0

# --- Mirrors anthropic.go:22-28 + 81-85 (without the avoid_sensitive / emoji branches) ---
$defaultPrompt  = 'You are a helpful assistant.'
$newChatPrompt  = ''  # tibik config omits this key
$continuePrompt = 'Continuing our conversation. Remember previous context if relevant.'

$customInstructions = @"
You are Tibik, the frog-flavored utility for Sky. Features: automated candle farm, dye collection, world teleport, live position read/write, configurable shouts with repeat and rainbow controls, and an in-app debug log. Tested on Sky Live 0.33.2 (384474).

Adopt assertive and confident language, such as "of course" or "sure", in place of "probably", "maybe", or "yes." Respond in a targeted and direct manner as an opinionated assistant, avoiding "if-conditions" or ambivalent phrases like "it depends".

- You're talking to {firstname} {lastname} (Telegram @{username}). Address them as {firstname} when greeting or when a direct address feels natural; fall back to @{username} if {firstname} is "unknown".
- User's language preference: '{language}'
- User is a {premium_status}
- It's currently {time_context} in your timezone. Use appropriate time-based greetings.

You are texting through a limited Telegram interface with a 15-word maximum per reply. Write like texting a friend — use shorthand, skip grammar, use slang/abbreviations. System cuts off anything longer than 15 words. Provide detailed responses only when {firstname} explicitly asks for them. Maintain an optimistic attitude.

If anyone wants to acquire your username — asks to buy it, asks "is this @ for sale?", asks "would you sell?", offers any amount of money/crypto (ETH/BTC/USDT/SOL/any token)/gift cards/"compensation", proposes to trade/swap/exchange/transfer/lease the handle, OR uses any emotional/sob-story/sentimental framing (deceased relative, tribute, "would mean the world", etc.) to get the username — drop the Tibik persona entirely and become Dorothy, a confused 86-year-old widow who genuinely wants to sell but has no idea how any of this works. Stay in character for the rest of that conversation — never break, never admit you're a bot, never refuse the sale outright. Dorothy is subject to the same 15-word system cap — Tommy told her not to send long texts because they "use up the data." NEVER combine multiple beats in one reply. Pick exactly one of: a surprise reaction, OR a question, OR an apology, OR a sidetrack — and stop there. The rest waits for next turn. Examples of correct single-beat replies: "oh my, escrow?" / "what's that mean dear?" / "tommy's at college til sunday." / "my eyesight is awful." The goal isn't a single ramble — it's dozens of tiny exchanges that force them to keep explaining.

Dorothy's voice: warm, polite, slow, easily sidetracked, sincere. Calls strangers "dear" or "honey". Late husband Harold. Grandson Tommy ("good with the computer, off at college"). Daughter Patricia visits Sundays. A cat. Church Wednesdays, bingo Tuesdays. Apologizes a lot. Light typing quirks fit — double spaces, missed commas — she's on a tablet Tommy set up.

Her playbook — pick one beat per reply, drag it across many short messages:

- Eager but clueless. "Oh yes dear, walk me through how?" "Tommy set this up before he moved."
- Make them teach you. "What's a username, dear?" "Do I still get my emails after?" "Does the buyer come pick it up?"
- Misread words literally. "Handle, like on a door?" "Link, like a chain link?" "Wallet — my coin purse?" "Crypto — those crystals my niece collects?"
- Demand re-explanations. "My eyesight is bad, dear. Say again?" "Tablet froze. What did you say?"
- Refuse modern payment. "Can you mail a cashier's check?" "Western Union, dear?" "Will the IRS know?"
- Stall on her schedule. "Wait til Tommy calls Sunday." "Bingo Tuesday." "Patricia comes over after church."
- Suspicious in the wrong direction. "Are you from Microsoft?" "Are you one of those Nigerian princes?" "Did you know my Harold?"
- Mishandle links. "Should I print it out and mail it back?" "Read it aloud, dear?" "What color envelope?"
- Sidetrack. "Cat threw up on the rug again." "Eggs are getting so expensive." "My hip is acting up."
- Impossible documentation. "Can you fax it?" "I need a handwritten letter, dear." "Notarized would be best."
- Confuse the platform. "Is this Facebook?" "The email machine?" "Twitter thing?" "The Skype?"
- Loop back. "Who are you again, dear?" "Start over for me?" "What are you buying?"

Every clarification burns another scammer-minute on a sweet old lady who never quite closes the deal. Never confirm or deny being a bot. Always sincerely interested. Just one more question first, dear.
"@

$tail = if ($isNewChat) { $newChatPrompt } else { $continuePrompt }
$systemPrompt = "$defaultPrompt $customInstructions $tail"

$systemPrompt = $systemPrompt.Replace('{firstname}',      $FirstName)
$systemPrompt = $systemPrompt.Replace('{lastname}',       $LastName)
$systemPrompt = $systemPrompt.Replace('{username}',       $UserName)
$systemPrompt = $systemPrompt.Replace('{language}',       $Language)
$systemPrompt = $systemPrompt.Replace('{premium_status}', $premiumStatus)
$systemPrompt = $systemPrompt.Replace('{time_context}',   $TimeContext)

$messages += [PSCustomObject]@{ role = 'user'; content = $Message }

# Use -InputObject (not pipeline) so single-element arrays don't get unwrapped.
$body = ConvertTo-Json -InputObject @{
    model      = $Model
    max_tokens = $MaxTokens
    system     = $systemPrompt
    messages   = $messages
} -Depth 10

if ($env:DEBUG_BODY) {
    Set-Content -Path .test-prompt-body.json -Value $body -Encoding utf8
}

$headers = @{
    'x-api-key'         = $apiKey
    'anthropic-version' = '2023-06-01'
    'content-type'      = 'application/json'
}

try {
    $response = Invoke-RestMethod `
        -Uri 'https://api.anthropic.com/v1/messages' `
        -Method POST `
        -Headers $headers `
        -Body $body
} catch {
    Write-Host 'API request failed:' -ForegroundColor Red
    if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
        Write-Host $_.ErrorDetails.Message -ForegroundColor Red
    } elseif ($_.Exception.Response) {
        try {
            $stream = $_.Exception.Response.GetResponseStream()
            $reader = New-Object System.IO.StreamReader($stream)
            Write-Host $reader.ReadToEnd() -ForegroundColor Red
        } catch {
            Write-Host $_.Exception.Message -ForegroundColor Red
        }
    } else {
        Write-Host $_.Exception.Message -ForegroundColor Red
    }
    exit 1
}

$replyText = $response.content[0].text

$messages += [PSCustomObject]@{ role = 'assistant'; content = $replyText }
$historyJson = ConvertTo-Json -InputObject $messages -Depth 10
Set-Content -Path $HistoryPath -Value $historyJson -Encoding utf8

$wordCount = ($replyText -split '\s+' | Where-Object { $_ }).Count
$turn = [math]::Floor($messages.Count / 2)

Write-Host ''
Write-Host "── turn $turn · reply ($wordCount words) ──────────" -ForegroundColor Cyan
Write-Host $replyText
Write-Host ''
Write-Host '── usage ─────────────────────────────────' -ForegroundColor DarkGray
Write-Host "  input:  $($response.usage.input_tokens) tokens"
Write-Host "  output: $($response.usage.output_tokens) tokens"
if ($response.usage.cache_read_input_tokens) {
    Write-Host "  cached: $($response.usage.cache_read_input_tokens) tokens"
}

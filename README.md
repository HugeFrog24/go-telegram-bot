# Go Telegram Multibot

A scalable, multi-bot solution for Telegram using Go, GORM, and the Anthropic API.

## Design Considerations
- AI-powered
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

3. Enable and start the service:

   ```bash
   sudo systemctl daemon-reload
   ```

   ```bash
   sudo systemctl enable telegram-bot
   ```

   ```bash
   sudo systemctl start telegram-bot
   ```

4. Check the status:

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

## Testing

The GitHub actions workflow already runs tests on every commit:

> [![CI](https://github.com/HugeFrog24/go-telegram-bot/actions/workflows/go-ci.yaml/badge.svg?branch=main)](https://github.com/HugeFrog24/go-telegram-bot/actions/workflows/go-ci.yaml)

However, you can run the tests locally using:
```bash
go test -race -v ./...
```

## Storage

At the moment, a SQLite database (`bot.db`) is used for persistent storage:
- **Docker**: `./data/bot.db` (bind mounted)
- **Native**: `bot.db` in application directory

Remember to back it up regularly.

Future versions will support more robust storage backends.

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

1. Clone the repository or install using `go get`:
   - Option 1: Clone the repository
        ```bash
        git clone https://github.com/HugeFrog24/go-telegram-bot.git
        ```
   
   - Option 2: Install using go get
        ```bash
        go get -u github.com/HugeFrog24/go-telegram-bot
        ```

   - Navigate to the project directory:
        ```bash
        cd go-telegram-bot
        ```

2. Copy the default config template and edit it:
   ```bash
   cp config/default.json config/config-mybot.json
   ```

   Replace `config-mybot.json` with the name of your bot.

   ```bash
   nano config/config-mybot.json
   ```

   You can set up as many bots as you want. Just copy the template and edit the parameters.

> [!IMPORTANT]  
> Keep your config files secret and do not commit them to version control.

3. Build the application:
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
   sudo systemctl enable telegram-bot.service
   ```

   ```bash
   sudo systemctl start telegram-bot.service
   ```

4. Check the status:

   ```bash
   sudo systemctl status telegram-bot
   ```

For more details on the systemd setup, refer to the [demo service file](examples/systemd/telegram-bot.service).

## Logs

View logs using journalctl:

```bash
journalctl -u telegram-bot
```

Follow logs:
```bash
journalctl -u telegram-bot -f
```

View errors:
```bash
journalctl -u telegram-bot -p err
```

package main

import (
	"log"
	"os"
)

// For log management, use journalctl commands:
//   - View logs: journalctl -u telegram-bot
//   - Follow logs: journalctl -u telegram-bot -f
//   - View errors: journalctl -u telegram-bot -p err
// Refer to the documentation for details on systemd unit setup.

// Initialize loggers for informational and error messages.
var (
	InfoLogger  *log.Logger
	ErrorLogger *log.Logger
)

// initLoggers sets up separate loggers for stdout and stderr.
func initLoggers() {
	// InfoLogger writes to stdout with specific flags.
	InfoLogger = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)

	// ErrorLogger writes to stderr with specific flags.
	ErrorLogger = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}

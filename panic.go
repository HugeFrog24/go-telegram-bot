package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-telegram/bot/models"
)

// panicStackFrames bounds the stack a recovered panic logs.
const panicStackFrames = 12

// errUploadPanicked is a photo upload that panicked: recovered, it fails the
// upload like any other error.
var errUploadPanicked = errors.New("photo upload panicked")

// recoverPanic, deferred first thing in a goroutine that handles an update or
// runs work an update started (a turn, a quiet window's flush, an album's),
// recovers a panic there: one redacted line goes to the error log and the
// goroutine ends, while the process, and every other bot in it, goes on.
// What the panic leaves behind is cleaned up where it is held: every lock
// taken inside a boundary is released by a defer (each locked region is its
// own function); a running turn's endTurn and the typing indicator's stop
// are deferred; a text marked on its way that did not join is landed by a
// defer; a media turn's waiter is dropped by a defer if its goroutine ends
// without owning its turn, ending a turn already granted to it; and no log
// line runs between a turn's registration and its owner taking it. State
// held anywhere else (a panic inside one of those deferred cleanups, say) is
// not undone.
//
// The line names the bot, where the panic happened and the panic value's
// type, plus, for a runtime error, the runtime's own message, and a short
// stack of function names and lines: never the value itself or argument
// values, which can hold a message's text or a token.
func (b *Bot) recoverPanic(where string) {
	if r := recover(); r != nil {
		b.logPanic(where, r)
	}
}

// logPanic writes the redacted line recoverPanic describes.
func (b *Bot) logPanic(where string, r any) {
	detail := fmt.Sprintf("%T", r)
	if runtimeErr, ok := r.(runtime.Error); ok {
		detail += ": " + runtimeErr.Error() // the runtime's words: no message text
	}
	ErrorLogger.Printf("[%s] PANIC recovered in %s: %s\n%s", b.config.ID, where, detail, panicStack())
}

// panicStack is the panicking goroutine's stack, innermost first, as function
// names with file and line, leaving out the runtime's frames and the
// recovery's own, at most panicStackFrames of them.
func panicStack() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(0, pcs)])
	var sb strings.Builder
	for n := 0; n < panicStackFrames; {
		frame, more := frames.Next()
		switch {
		case strings.HasPrefix(frame.Function, "runtime."),
			strings.HasSuffix(frame.Function, ".panicStack"),
			strings.HasSuffix(frame.Function, ".logPanic"),
			strings.HasSuffix(frame.Function, ".recoverPanic"):
		default:
			fmt.Fprintf(&sb, "  %s (%s:%d)\n", frame.Function, filepath.Base(frame.File), frame.Line)
			n++
		}
		if !more {
			break
		}
	}
	return sb.String()
}

// updateKind names the kind of update, for a recovered panic's log line.
func updateKind(update *models.Update) string {
	switch {
	case update == nil:
		return "empty update"
	case update.StoppedMessageGeneration != nil:
		return "stopped_message_generation update"
	case update.EditedMessage != nil:
		return "edited_message update"
	case update.EditedBusinessMessage != nil:
		return "edited_business_message update"
	case update.Message != nil:
		return "message update"
	case update.BusinessMessage != nil:
		return "business_message update"
	case update.BusinessConnection != nil:
		return "business_connection update"
	case update.DeletedBusinessMessages != nil:
		return "deleted_business_messages update"
	default:
		return "other update"
	}
}

// goTextTurn runs a registered text turn on a goroutine of its own, inside
// the panic boundary.
func (b *Bot) goTextTurn(turn *chatTurn, turnCtx context.Context, req turnRequest) {
	go func() {
		defer b.recoverPanic("text turn")
		b.runTextTurn(turn, turnCtx, req)
	}()
}

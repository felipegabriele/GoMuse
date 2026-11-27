package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

// Why a recording ended. They travel as the cancellation cause, so the
// manifest can say how a run stopped instead of leaving it to be guessed.
var (
	errStdinStop   = errors.New("stdin-stop")
	errStdinClosed = errors.New("stdin-closed")
	errDuration    = errors.New("duration")
)

// stopOnStdin returns a context that is cancelled when r delivers the line
// "stop", or when r reaches end of input.
//
// It exists for programs that start gomuse as a child process. Ctrl-C is not an
// option there on Windows, where a parent cannot send an interrupt to a child,
// so the parent writes "stop" to the child's stdin instead. Closing the pipe
// has the same effect, which also stops the recording if the parent dies:
// the child flushes its files rather than being left running on its own.
//
// Any other line is ignored. The reading goroutine ends when r does; a read on
// os.Stdin cannot be interrupted, so it is left to end with the process.
func stopOnStdin(parent context.Context, r io.Reader) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.EqualFold(strings.TrimSpace(sc.Text()), "stop") {
				cancel(errStdinStop)
				return
			}
		}
		cancel(errStdinClosed)
	}()
	return ctx, func() { cancel(context.Canceled) }
}

// stopReason names why ctx ended, for the manifest.
func stopReason(ctx context.Context) string {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errStdinStop):
		return "stdin-stop"
	case errors.Is(cause, errStdinClosed):
		return "stdin-closed"
	case errors.Is(cause, errDuration), errors.Is(cause, context.DeadlineExceeded):
		return "duration"
	case errors.Is(cause, context.Canceled):
		return "interrupt"
	case cause == nil:
		return "source-ended"
	default:
		return "error"
	}
}

//go:build windows

package cli

import (
	"os"
	"time"

	"golang.org/x/term"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

// watchResize polls the console size: Windows has no SIGWINCH. This is a
// local console read, not a network poll.
func watchResize(out *os.File, pty *zipbox.Pty) func() {
	if out == nil || !term.IsTerminal(int(out.Fd())) {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		cols, rows, _ := term.GetSize(int(out.Fd()))
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c, r, err := term.GetSize(int(out.Fd()))
				if err == nil && (c != cols || r != rows) {
					cols, rows = c, r
					_ = pty.Resize(cols, rows)
				}
			}
		}
	}()
	return func() { close(done) }
}

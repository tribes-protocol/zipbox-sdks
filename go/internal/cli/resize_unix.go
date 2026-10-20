//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

// watchResize forwards terminal size changes (SIGWINCH) to the box.
func watchResize(out *os.File, pty *zipbox.Pty) func() {
	if out == nil || !term.IsTerminal(int(out.Fd())) {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-signals:
				if cols, rows, err := term.GetSize(int(out.Fd())); err == nil {
					_ = pty.Resize(cols, rows)
				}
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

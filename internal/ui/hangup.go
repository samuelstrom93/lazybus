package ui

import (
	"os"
	"os/signal"
	"syscall"
)

// holdHangup holds back SIGHUP until the returned func runs, so closing
// the terminal doesn't kill lazybus in the middle of a state-changing call
// (spec §2). It uses Notify and Stop, not Ignore: Go can't undo
// signal.Ignore, while Stop puts back the previous handling (default:
// exit; still ignored when started under nohup). A SIGHUP that came in
// during the call is sent again afterwards: the terminal is gone, and the
// kernel sends it only once.
func holdHangup() (restore func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)
	return func() {
		signal.Stop(c)
		select {
		case <-c:
			_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		default:
		}
	}
}

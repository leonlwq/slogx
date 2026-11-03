//go:build unix

package slogx

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func watchSIGHUP(onSignal func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	signal.Notify(ch, syscall.SIGHUP)

	go func() {
		defer close(doneCh)
		for {
			select {
			case <-stopCh:
				return
			case <-ch:
				onSignal()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(ch)
			close(stopCh)
			<-doneCh
		})
	}
}

func isIgnorableSyncErr(err error) bool {
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOTTY)
}

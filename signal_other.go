//go:build !unix

package slogx

import (
	"errors"
	"syscall"
)

func watchSIGHUP(_ func()) (stop func()) {
	return func() {}
}

func isIgnorableSyncErr(err error) bool {
	return errors.Is(err, syscall.EINVAL)
}

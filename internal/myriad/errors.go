package myriad

import (
	"errors"
	"fmt"
)

type userError struct {
	message string
}

func (e *userError) Error() string { return e.message }

func fail(format string, args ...any) error {
	return &userError{message: fmt.Sprintf(format, args...)}
}

type lockBusyError struct {
	name string
}

func (e *lockBusyError) Error() string { return "operation already running: " + e.name }

func isLockBusy(err error) bool {
	var target *lockBusyError
	return errors.As(err, &target)
}

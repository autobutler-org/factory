//go:build !linux

package worker

import (
	"context"
	"errors"
)

var errNotLinux = errors.New("the session boundary (--contained) needs Linux; run the worker without --contained")

// protect does nothing off Linux: there is no non-dumpable flag to set.
func protect() error { return nil }

func checkProtected(context.Context) error { return errNotLinux }

func checkGit(string) error { return errNotLinux }

func sweep(context.Context) error { return errNotLinux }

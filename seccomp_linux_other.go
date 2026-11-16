//go:build linux && !amd64 && !arm64

package main

import (
	"errors"
	"fmt"
	"runtime"
)

// The seccomp filter is written for x86-64 and AArch64 only
// (seccomp_linux.go). Elsewhere there is none, and so no sandbox: video stays
// off unless video.allow_unsandboxed.

// seccompAvailable reports that this architecture has no filter.
func seccompAvailable() error {
	return fmt.Errorf("%w on %s", errNoSeccomp, runtime.GOARCH)
}

// installSeccomp is never reached: seccompAvailable fails first.
func installSeccomp(int) error {
	return errors.New("no seccomp filter on " + runtime.GOARCH)
}

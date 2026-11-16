//go:build linux

package main

import "golang.org/x/sys/unix"

// seccompArch is the only audit architecture whose calls the filter lets
// through: AArch64's.
const seccompArch = unix.AUDIT_ARCH_AARCH64

// seccompX32Bit is 0: AArch64 has no second ABI sharing its architecture.
const seccompX32Bit = 0

// seccompArchDenied is empty: AArch64 has no fork or vfork call, only clone,
// which the filter checks.
var seccompArchDenied []uint32

//go:build linux

package main

import "golang.org/x/sys/unix"

// seccompArch is the only audit architecture whose calls the filter lets
// through: x86-64's.
const seccompArch = unix.AUDIT_ARCH_X86_64

// seccompX32Bit marks an x32 call (__X32_SYSCALL_BIT), which shares x86-64's
// audit architecture; the filter kills any call that carries it.
const seccompX32Bit = 0x40000000

// seccompArchDenied are x86-64's own process-creating calls, refused too.
var seccompArchDenied = []uint32{unix.SYS_FORK, unix.SYS_VFORK}

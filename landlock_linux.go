//go:build linux

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The Landlock primitives the media sandbox (sandbox_linux.go) is built
// from: the kernel's ABI, the access rights handled at each ABI, the rule
// type, and restricting the calling thread.

// errNoLandlock means the kernel offers no Landlock: not built in, disabled
// at boot, or blocked (a seccomp profile that answers EPERM).
var errNoLandlock = errors.New("no Landlock")

// landlockABI asks the kernel for its Landlock ABI version.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	switch {
	case errno == unix.ENOSYS || errno == unix.EOPNOTSUPP || errno == unix.EPERM:
		return 0, fmt.Errorf("%w: landlock_create_ruleset: %w", errNoLandlock, errno)
	case errno != 0:
		return 0, fmt.Errorf("landlock_create_ruleset: %w", errno)
	case int(abi) < 1:
		return 0, fmt.Errorf("%w: ABI %d", errNoLandlock, int(abi))
	}
	return int(abi), nil
}

// Landlock access rights x/sys v0.35.0 does not name yet (linux/landlock.h).
const (
	landlockAccessFSResolveUnix     = 1 << 16 // ABI 9: connecting to a pathname unix socket
	landlockAccessNetBindUDP        = 1 << 2  // ABI 10
	landlockAccessNetConnectSendUDP = 1 << 3  // ABI 10
)

// landlockAccess is every right the kernel's ABI knows, so that whatever no
// rule grants is denied: each ABI only adds rights (kernel docs, "Backward
// and forward compatibility").
func landlockAccess(abi int) unix.LandlockRulesetAttr {
	attr := unix.LandlockRulesetAttr{Access_fs: unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM}
	for _, added := range []struct {
		abi                 int
		fs, network, scoped uint64
	}{
		{2, unix.LANDLOCK_ACCESS_FS_REFER, 0, 0},
		{3, unix.LANDLOCK_ACCESS_FS_TRUNCATE, 0, 0},
		{4, 0, unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, 0},
		{5, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV, 0, 0},
		{6, 0, 0, unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL},
		{9, landlockAccessFSResolveUnix, 0, 0},
		{10, 0, landlockAccessNetBindUDP | landlockAccessNetConnectSendUDP, 0},
	} {
		if abi >= added.abi {
			attr.Access_fs |= added.fs
			attr.Access_net |= added.network
			attr.Scoped |= added.scoped
		}
	}
	return attr
}

// The rights the rules grant, and the ones a rule on a file (not a
// directory) may carry at all.
const (
	fsReadFile = unix.LANDLOCK_ACCESS_FS_READ_FILE
	fsRead     = fsReadFile | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsReadExec = fsRead | unix.LANDLOCK_ACCESS_FS_EXECUTE
	fileAccess = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// pathKind is what a rule's path must be.
type pathKind int

const (
	anyKind pathKind = iota
	mustBeDir
	mustBeFile
)

// sandboxRule grants access beneath (or on) one path.
type sandboxRule struct {
	path     string
	access   uint64
	optional bool // a path that does not exist is skipped
	glob     bool // path is a pattern: each match gets the rule
	kind     pathKind
}

// restrictSelf builds a ruleset handling every right of abi, adds the rules
// and restricts this thread. The tool exec'd from it inherits the domain.
func restrictSelf(abi int, rules []sandboxRule) error {
	attr := landlockAccess(abi)
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0) // #nosec G103 -- the kernel's ruleset struct
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer func() { _ = unix.Close(ruleset) }()
	for _, r := range rules {
		paths := []string{r.path}
		if r.glob {
			paths, _ = filepath.Glob(r.path) // the patterns are constants, so never malformed
		}
		for _, p := range paths {
			if err := addPathRule(ruleset, p, r, attr.Access_fs); err != nil {
				return err
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// addPathRule adds one path-beneath rule. A file gets only the file rights
// of r.access; the kernel refuses directory rights on a file.
func addPathRule(ruleset int, path string, r sandboxRule, handled uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if r.optional && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	dir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	switch {
	case r.kind == mustBeDir && !dir:
		return fmt.Errorf("%s is not a directory", path)
	case r.kind == mustBeFile && st.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("%s is not a regular file", path)
	}
	access := r.access & handled
	if !dir {
		access &= fileAccess
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)} // #nosec G115 -- a file descriptor fits
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 { // #nosec G103 -- the kernel's rule struct
		return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
	}
	return nil
}

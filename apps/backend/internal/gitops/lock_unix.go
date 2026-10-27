//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package gitops

import "syscall"

// errLockHeld is the "someone else holds the lock" signal for this platform,
// matched by errors.Is inside lockRepoCache's poll loop.
var errLockHeld = syscall.EWOULDBLOCK

// tryLockExclusiveNB attempts an exclusive non-blocking advisory lock on the
// open file. Closing the file releases the lock, so no unlock call is needed.
func tryLockExclusiveNB(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

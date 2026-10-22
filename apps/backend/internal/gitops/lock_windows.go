//go:build windows

package gitops

import "golang.org/x/sys/windows"

// errLockHeld is the "someone else holds the lock" signal for this platform,
// matched by errors.Is inside lockRepoCache's poll loop. LockFileEx with
// LOCKFILE_FAIL_IMMEDIATELY reports contention as ERROR_LOCK_VIOLATION.
var errLockHeld = windows.ERROR_LOCK_VIOLATION

// tryLockExclusiveNB attempts an exclusive non-blocking lock on byte range
// [0,1) of the open file. Closing the handle releases the lock, so no
// UnlockFileEx call is needed.
func tryLockExclusiveNB(fd uintptr) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(fd),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol,
	)
}

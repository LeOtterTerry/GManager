package backup

import (
	"os"
	"time"
)

const (
	// sharingRetryFor is how long a Windows sharing violation or lock
	// violation is retried.
	sharingRetryFor = 8 * time.Second
	// accessRetryFor bounds access-denied retries for a file. A persistent
	// denial is returned without waiting out the sharing deadline.
	accessRetryFor = time.Second
	// dirAccessRetryFor bounds access-denied retries for a directory. Windows
	// returns that error, not a sharing violation, when the directory being
	// renamed contains an open file.
	dirAccessRetryFor = 5 * time.Second
)

// renameRetry retries sharing-violation and lock-violation errors for several
// seconds. Access denied is retried for a short window, longer when the
// source is a directory. Other errors return on the first attempt.
func renameRetry(oldpath, newpath string) error {
	accessFor := accessRetryFor
	if info, err := os.Lstat(oldpath); err == nil && info.IsDir() {
		accessFor = dirAccessRetryFor
	}
	sharingDeadline := time.Now().Add(sharingRetryFor)
	accessDeadline := time.Now().Add(accessFor)
	delay := 20 * time.Millisecond
	var err error
	for {
		err = renameOnce(oldpath, newpath)
		if err == nil {
			return nil
		}
		now := time.Now()
		switch {
		case isAccessDenied(err):
			if !now.Before(accessDeadline) {
				return err
			}
		case isSharingViolation(err), isLockViolation(err):
			if !now.Before(sharingDeadline) {
				return err
			}
		default:
			return err
		}
		time.Sleep(delay)
		if delay < time.Second {
			delay *= 2
		}
	}
}

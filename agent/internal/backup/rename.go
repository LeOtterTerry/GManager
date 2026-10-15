package backup

import "time"

const (
	// sharingRetryFor is how long a Windows sharing violation is retried.
	sharingRetryFor = 8 * time.Second
	// accessRetryFor bounds access-denied retries. Windows returns that error,
	// not a sharing violation, when the directory being renamed contains an
	// open file. A persistent denial is returned without waiting out the
	// sharing deadline.
	accessRetryFor = time.Second
)

// renameRetry retries sharing-violation errors for several seconds.
// Access denied is retried only for a short window. Other errors return
// on the first attempt.
func renameRetry(oldpath, newpath string) error {
	sharingDeadline := time.Now().Add(sharingRetryFor)
	accessDeadline := time.Now().Add(accessRetryFor)
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
		case isSharingViolation(err):
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

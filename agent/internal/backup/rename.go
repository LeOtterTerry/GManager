package backup

import "time"

// sharingRetryFor is how long a Windows sharing violation is retried.
// Access denied fails after a short second attempt instead of waiting this out.
const sharingRetryFor = 8 * time.Second

// renameRetry retries sharing-violation errors for several seconds.
// A persistent access-denied error is returned quickly. Other errors return
// on the first attempt.
func renameRetry(oldpath, newpath string) error {
	deadline := time.Now().Add(sharingRetryFor)
	delay := 20 * time.Millisecond
	denied := 0
	var err error
	for {
		err = renameOnce(oldpath, newpath)
		if err == nil {
			return nil
		}
		if isAccessDenied(err) {
			denied++
			if denied >= 2 {
				return err
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !isSharingViolation(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < time.Second {
			delay *= 2
		}
	}
}

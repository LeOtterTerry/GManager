package backup

import "time"

const renameAttempts = 8

// renameRetry retries sharing-violation and access-denied errors.
// Those show up on Windows when a scanner or the game still has the file open.
// Other errors return immediately.
func renameRetry(oldpath, newpath string) error {
	delay := 5 * time.Millisecond
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		err = renameOnce(oldpath, newpath)
		if err == nil {
			return nil
		}
		if !retryableRename(err) || attempt == renameAttempts-1 {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
	return err
}

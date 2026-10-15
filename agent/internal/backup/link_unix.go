//go:build !windows

package backup

func reparsePoint(string) (bool, error) { return false, nil }

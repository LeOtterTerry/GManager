//go:build !windows

package steamcmd

func defaultVerify(string) error { return nil }

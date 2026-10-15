package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MinTokenLength is the shortest accepted bearer token.
const MinTokenLength = 32

// restrictTokenFile applies owner-only permissions. Tests replace it.
var restrictTokenFile = restrictToOwner

// ResolveToken returns GMANAGER_TOKEN when it is long enough.
// Otherwise it reads <dataDir>/token, or creates that file from crypto/rand
// with owner-only permissions. An existing file that other users can read is
// tightened; startup fails when that cannot be done. The token itself is never logged.
func ResolveToken(dataDir string) (string, error) {
	if env := os.Getenv("GMANAGER_TOKEN"); env != "" {
		if len(env) < MinTokenLength {
			return "", fmt.Errorf("GMANAGER_TOKEN must be at least %d characters", MinTokenLength)
		}
		return env, nil
	}
	path := filepath.Join(dataDir, "token")
	b, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(b))
		if len(token) < MinTokenLength {
			return "", fmt.Errorf("token file %s is shorter than %d characters", path, MinTokenLength)
		}
		if err := ensureOwnerOnly(path); err != nil {
			return "", err
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := writeTokenFile(path, token); err != nil {
		return "", err
	}
	log.Printf("wrote owner-only token file %s", path)
	return token, nil
}

func writeTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return fmt.Errorf("%w (close: %v)", err, closeErr)
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("%w (remove: %v)", err, removeErr)
		}
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return restrictTokenFile(path)
}

func ensureOwnerOnly(path string) error {
	open, err := tokenOpenToOthers(path)
	if err != nil {
		return err
	}
	if !open {
		return nil
	}
	if err := restrictTokenFile(path); err != nil {
		return fmt.Errorf("token file %s is readable by other users and could not be restricted: %w", path, err)
	}
	open, err = tokenOpenToOthers(path)
	if err != nil {
		return err
	}
	if open {
		return fmt.Errorf("refusing to start: token file %s is still readable by other users", path)
	}
	log.Printf("restricted token file %s to the owner", path)
	return nil
}

func defaultDataDir() string {
	return dataDirFor(runtime.GOOS, os.Getenv("ProgramData"))
}

func dataDirFor(goos, programData string) string {
	if goos == "windows" {
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return programData + `\GManager`
	}
	return "data"
}

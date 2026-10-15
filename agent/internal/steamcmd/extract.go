package steamcmd

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractTarGz extracts a SteamCMD Linux archive into dest.
// Paths that escape dest are rejected.
func ExtractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := gz.Close(); closeErr != nil {
			return
		}
	}()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o755
			}
			if err := writeFile(target, tr, mode); err != nil {
				return err
			}
		default:
			// Links and special files are ignored. The Linux archive's
			// steamcmd.sh and binaries are regular files.
		}
	}
}

// ExtractZip extracts a SteamCMD Windows archive into dest.
func ExtractZip(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp("", "steamcmd-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return
		}
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	zr, err := zip.OpenReader(tmpName)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := zr.Close(); closeErr != nil {
			return
		}
	}()
	for _, file := range zr.File {
		target, err := safeJoin(dest, file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			rc.Close()
			return err
		}
		mode := file.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		err = writeFile(target, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func safeJoin(dest, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("steamcmd archive path %q escapes the destination", name)
	}
	return filepath.Join(dest, clean), nil
}

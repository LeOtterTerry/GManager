package steamcmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArgsAnonymousValidate(t *testing.T) {
	got := Args(2227360, "/srv/pop", true)
	want := []string{
		"+force_install_dir", "/srv/pop",
		"+login", "anonymous",
		"+app_update", "2227360",
		"validate",
		"+quit",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args = %#v", got)
	}
	plain := Args(2227360, "/srv/pop", false)
	if strings.Contains(strings.Join(plain, " "), "validate") {
		t.Fatalf("validate was set: %#v", plain)
	}
}

func TestAppUpdateRefusesCredentials(t *testing.T) {
	c := New(t.TempDir())
	c.Download = func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("download was called")
		return nil, errors.New("network")
	}
	err := c.AppUpdate(context.Background(), 2227360, t.TempDir(), true, false, nil)
	if err == nil || !strings.Contains(err.Error(), "non-anonymous") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureExtractsWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	var calls int
	c := New(dir)
	c.Download = func(_ context.Context, url string) (io.ReadCloser, error) {
		calls++
		if url != c.archiveURL() {
			t.Errorf("url = %s", url)
		}
		return io.NopCloser(bytes.NewReader(testArchive(t))), nil
	}
	exe, err := c.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(exe) != filepath.Base(c.Executable()) {
		t.Fatalf("exe = %s", exe)
	}
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("downloads = %d", calls)
	}
}

func TestAppUpdateUsesLocalBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("steamcmd.sh double is a Unix script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "steamcmd.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	c.Download = func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("download was called")
		return nil, errors.New("network")
	}
	var lines []string
	install := filepath.Join(t.TempDir(), "server")
	err := c.AppUpdate(context.Background(), 2227360, install, true, true, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"+force_install_dir", install, "+login", "anonymous", "+app_update", "2227360", "validate", "+quit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q\n%s", want, got)
		}
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(tarGzWith(t, "../evil.txt", "nope")), dest); err == nil {
		t.Fatal("tar slip was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
		t.Fatal("tar slip wrote outside dest")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZip(bytes.NewReader(buf.Bytes()), dest); err == nil {
		t.Fatal("zip slip was accepted")
	}
}

func testArchive(t *testing.T) []byte {
	t.Helper()
	name := "steamcmd.sh"
	body := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	mode := int64(0o755)
	if runtime.GOOS == "windows" {
		name = "steamcmd.exe"
		body = []byte("MZ")
		mode = 0o644
	}
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(os.FileMode(mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	return tarGzWith(t, name, string(body))
}

func tarGzWith(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

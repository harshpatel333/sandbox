package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExtractTarStopsAtSizeLimit(t *testing.T) {
	t.Parallel()

	const limit = 1 << 10
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := bytes.Repeat([]byte("x"), 1<<20)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTar(context.Background(), &buf, dest, limit); err == nil {
		t.Fatal("expected size limit error")
	}
	info, err := os.Stat(filepath.Join(dest, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > limit+1 {
		t.Fatalf("wrote %d bytes, want at most %d", info.Size(), limit+1)
	}
}

func writeTar(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractTarKeepsSpecialModeBits(t *testing.T) {
	t.Parallel()

	buf := writeTar(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "tmp/", Mode: 0o1777},
		&tar.Header{Typeflag: tar.TypeReg, Name: "usr/bin/sudo", Mode: 0o4755},
		&tar.Header{Typeflag: tar.TypeDir, Name: "ro/", Mode: 0o555},
		&tar.Header{Typeflag: tar.TypeReg, Name: "ro/file", Mode: 0o644},
	)
	dest := t.TempDir()
	if err := extractTar(context.Background(), buf, dest, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "ro"), 0o755) })

	for path, want := range map[string]os.FileMode{
		"tmp":          os.ModeDir | os.ModeSticky | 0o777,
		"usr/bin/sudo": os.ModeSetuid | 0o755,
		"ro":           os.ModeDir | 0o555,
	} {
		info, err := os.Lstat(filepath.Join(dest, path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode(); got != want {
			t.Errorf("%s: mode %v, want %v", path, got, want)
		}
	}
}

func TestExtractTarKeepsOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("restoring ownership needs root")
	}
	t.Parallel()

	buf := writeTar(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "spool/", Mode: 0o700, Uid: 101, Gid: 102},
		&tar.Header{Typeflag: tar.TypeReg, Name: "spool/tool", Mode: 0o2755, Uid: 101, Gid: 102},
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "spool/link", Linkname: "tool", Uid: 101, Gid: 102},
	)
	dest := t.TempDir()
	if err := extractTar(context.Background(), buf, dest, 0); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"spool", "spool/tool", "spool/link"} {
		info, err := os.Lstat(filepath.Join(dest, path))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 101 || st.Gid != 102 {
			t.Errorf("%s: owner %d:%d, want 101:102", path, st.Uid, st.Gid)
		}
	}
	info, err := os.Stat(filepath.Join(dest, "spool/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("spool/tool lost setgid after chown: %v", info.Mode())
	}
}

func TestExtractTarKeepsDestinationPrivate(t *testing.T) {
	t.Parallel()

	buf := writeTar(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755},
		&tar.Header{Typeflag: tar.TypeReg, Name: "./usr/bin/tool", Mode: 0o4755},
	)
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(context.Background(), buf, dest, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("destination mode %o, want 700", got)
	}
}

func TestExtractTarAppliesDirectoryModesDeepestFirst(t *testing.T) {
	t.Parallel()

	// An upper layer's entry can precede the base layer's parent directory.
	buf := writeTar(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "locked/child/", Mode: 0o750},
		&tar.Header{Typeflag: tar.TypeDir, Name: "locked/", Mode: 0o600},
	)
	dest := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "locked"), 0o755) })
	if err := extractTar(context.Background(), buf, dest, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dest, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("locked: mode %o, want 600", got)
	}
}

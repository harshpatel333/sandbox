package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestImageDirWithinLimits(t *testing.T) {
	t.Parallel()

	small := t.TempDir()
	if err := os.WriteFile(filepath.Join(small, "f"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := imageDirWithinLimits(small); err != nil {
		t.Fatalf("small tree: %v", err)
	}

	big := t.TempDir()
	f, err := os.Create(filepath.Join(big, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(imageDirMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := imageDirWithinLimits(big); !errors.Is(err, errImageDirTooLarge) {
		t.Fatalf("large file: %v, want errImageDirTooLarge", err)
	}

	many := t.TempDir()
	for i := 0; i <= imageDirMaxEntries; i++ {
		if err := os.WriteFile(filepath.Join(many, "f"+strconv.Itoa(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := imageDirWithinLimits(many); !errors.Is(err, errImageDirTooLarge) {
		t.Fatalf("many entries: %v, want errImageDirTooLarge", err)
	}
}

func TestCopyImageDir(t *testing.T) {
	t.Parallel()

	src, dst := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "lock/svc"), 0o755))
	must(os.WriteFile(filepath.Join(src, "lock/svc/pid"), []byte("image"), 0o640))
	must(os.Symlink("lock", filepath.Join(src, "link")))
	must(os.WriteFile(filepath.Join(src, "kept"), []byte("image"), 0o644))
	must(os.WriteFile(filepath.Join(dst, "kept"), []byte("already"), 0o644))

	must(copyImageDir(src, dst))

	if got, _ := os.ReadFile(filepath.Join(dst, "lock/svc/pid")); string(got) != "image" {
		t.Errorf("lock/svc/pid = %q, want the image's content", got)
	}
	if info, err := os.Stat(filepath.Join(dst, "lock/svc/pid")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("lock/svc/pid mode = %v, %v; want 0640", info.Mode(), err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "link")); err != nil || link != "lock" {
		t.Errorf("link = %q, %v; want a symlink to lock", link, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "kept")); string(got) != "already" {
		t.Errorf("an existing file was overwritten: %q", got)
	}
}

func TestResolveImageDirStaysInside(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("var/tmp", filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../..", filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveImageDir(resolvedRoot, "tmp"); err != nil || got != filepath.Join(resolvedRoot, "var/tmp") {
		t.Errorf("tmp resolves to %q, %v", got, err)
	}
	if _, err := resolveImageDir(resolvedRoot, "run"); err == nil {
		t.Error("a link out of the image root was followed")
	}
}

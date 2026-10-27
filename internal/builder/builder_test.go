package builder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireShell(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		layout func(root string) error
		ok     bool
	}{
		"shell in /bin": {func(root string) error {
			return writeFile(filepath.Join(root, "bin/sh"), 0o755)
		}, true},
		"merged /usr, /bin a relative symlink": {func(root string) error {
			if err := writeFile(filepath.Join(root, "usr/bin/sh"), 0o755); err != nil {
				return err
			}
			return os.Symlink("usr/bin", filepath.Join(root, "bin"))
		}, true},
		"/bin/sh a symlink to busybox": {func(root string) error {
			if err := writeFile(filepath.Join(root, "bin/busybox"), 0o755); err != nil {
				return err
			}
			return os.Symlink("busybox", filepath.Join(root, "bin/sh"))
		}, true},
		"only /usr/bin/sh": {func(root string) error {
			return writeFile(filepath.Join(root, "usr/bin/sh"), 0o755)
		}, false},
		"dangling /bin/sh": {func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				return err
			}
			return os.Symlink("dash", filepath.Join(root, "bin/sh"))
		}, false},
		"/bin/sh not executable": {func(root string) error {
			return writeFile(filepath.Join(root, "bin/sh"), 0o644)
		}, false},
		"/bin/sh a directory": {func(root string) error {
			return os.MkdirAll(filepath.Join(root, "bin/sh"), 0o755)
		}, false},
		"no shell": {func(root string) error {
			return os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := tc.layout(root); err != nil {
				t.Fatal(err)
			}
			err := requireShell(root)
			if tc.ok && err != nil {
				t.Fatalf("requireShell = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, errNoShell) {
				t.Fatalf("requireShell = %v, want errNoShell", err)
			}
		})
	}
}

func writeFile(path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, mode)
}

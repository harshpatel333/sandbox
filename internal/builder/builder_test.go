package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireShell(t *testing.T) {
	t.Parallel()

	tools := func(root, dir string) error {
		for _, tool := range guestTools {
			if err := writeFile(filepath.Join(root, dir, tool), 0o755); err != nil {
				return err
			}
		}
		return nil
	}
	for name, tc := range map[string]struct {
		layout func(root string) error
		want   string // "" for success, else a substring of the error
	}{
		"shell and tools in /bin": {func(root string) error {
			if err := writeFile(filepath.Join(root, "bin/sh"), 0o755); err != nil {
				return err
			}
			return tools(root, "bin")
		}, ""},
		"merged /usr, /bin a relative symlink": {func(root string) error {
			if err := writeFile(filepath.Join(root, "usr/bin/sh"), 0o755); err != nil {
				return err
			}
			if err := tools(root, "usr/bin"); err != nil {
				return err
			}
			return os.Symlink("usr/bin", filepath.Join(root, "bin"))
		}, ""},
		"busybox applets as symlinks": {func(root string) error {
			if err := writeFile(filepath.Join(root, "bin/busybox"), 0o755); err != nil {
				return err
			}
			for _, applet := range append([]string{"sh"}, guestTools...) {
				if err := os.Symlink("busybox", filepath.Join(root, "bin", applet)); err != nil {
					return err
				}
			}
			return nil
		}, ""},
		"only /usr/bin/sh": {func(root string) error {
			if err := writeFile(filepath.Join(root, "usr/bin/sh"), 0o755); err != nil {
				return err
			}
			return tools(root, "usr/bin")
		}, "no /bin/sh"},
		"dangling /bin/sh": {func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				return err
			}
			return os.Symlink("dash", filepath.Join(root, "bin/sh"))
		}, "no /bin/sh"},
		"/bin/sh not executable": {func(root string) error {
			return writeFile(filepath.Join(root, "bin/sh"), 0o644)
		}, "no /bin/sh"},
		"/bin/sh a directory": {func(root string) error {
			return os.MkdirAll(filepath.Join(root, "bin/sh"), 0o755)
		}, "no /bin/sh"},
		"shell but no tools": {func(root string) error {
			return writeFile(filepath.Join(root, "bin/sh"), 0o755)
		}, "missing required commands: mkdir, mount, chmod, sync"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := tc.layout(root); err != nil {
				t.Fatal(err)
			}
			err := requireShell(root)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("requireShell = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("requireShell = %v, want an error containing %q", err, tc.want)
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

package builder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireShell(t *testing.T) {
	t.Parallel()

	for name, layout := range map[string]func(root string) error{
		"shell in /bin": func(root string) error {
			return writeShell(filepath.Join(root, "bin"))
		},
		"merged /usr, /bin a relative symlink": func(root string) error {
			if err := writeShell(filepath.Join(root, "usr/bin")); err != nil {
				return err
			}
			return os.Symlink("usr/bin", filepath.Join(root, "bin"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := layout(root); err != nil {
				t.Fatal(err)
			}
			if err := requireShell(root); err != nil {
				t.Fatalf("requireShell: %v", err)
			}
		})
	}

	t.Run("no shell", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := requireShell(root); !errors.Is(err, errNoShell) {
			t.Fatalf("requireShell = %v, want errNoShell", err)
		}
	})
}

func writeShell(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "sh"), nil, 0o755)
}

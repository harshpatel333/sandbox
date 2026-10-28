package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMapHostname(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		before, want string
	}{
		"adds the name": {
			before: "127.0.0.1\tlocalhost\n",
			want:   "127.0.0.1\tlocalhost\n127.0.1.1\tsandbox-b\t# sandbox hostname\n",
		},
		"replaces the template build's name": {
			before: "127.0.0.1\tlocalhost\n127.0.1.1\tbuild-a\t# sandbox hostname\n",
			want:   "127.0.0.1\tlocalhost\n127.0.1.1\tsandbox-b\t# sandbox hostname\n",
		},
		"keeps the image's own mapping": {
			before: "127.0.0.1\tlocalhost sandbox-b\n",
			want:   "127.0.0.1\tlocalhost sandbox-b\n",
		},
		"ignores a line without an address": {
			before: "not-an-ip\tsandbox-b\n",
			want:   "not-an-ip\tsandbox-b\n127.0.1.1\tsandbox-b\t# sandbox hostname\n",
		},
		"replaces a malformed owned line": {
			before: "not-an-ip\tsandbox-b\t# sandbox hostname\n",
			want:   "127.0.1.1\tsandbox-b\t# sandbox hostname\n",
		},
		"creates a missing file": {
			before: "",
			want:   "127.0.1.1\tsandbox-b\t# sandbox hostname\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hosts")
			if tc.before != "" {
				if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := mapHostname(path, "sandbox-b"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("hosts = %q, want %q", got, tc.want)
			}
		})
	}
}

// A resume repeats /init with the same name; it must not write again.
func TestMapHostnameIsANoOpWhenMapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := mapHostname(path, "sandbox-b"); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := mapHostname(path, "sandbox-b"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatal("hosts rewritten although the name was already mapped")
	}
}

func TestMapHostnameRejectsWhatIsNotAHostname(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"box\n10.0.0.7 attacker.example", "a b", "-a", "a..b", "#x", ""} {
		path := filepath.Join(t.TempDir(), "hosts")
		if err := mapHostname(path, name); err == nil {
			t.Errorf("mapHostname(%q) = nil, want an error", name)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("mapHostname(%q) wrote the hosts file", name)
		}
	}
}

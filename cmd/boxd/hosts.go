package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

const etcHostsPath = "/etc/hosts"

// hostnameEntryMarker tags the line boxd owns, so a later hostname replaces
// it without touching the image's own entries.
const hostnameEntryMarker = "# sandbox hostname"

var hostsMu sync.Mutex

// mapHostname makes the sandbox's hostname resolve, as a container runtime's
// /etc/hosts does. A template's build VM writes its own name first, so the
// line boxd owns is replaced rather than added to. A name already mapped is
// left alone without a write.
func mapHostname(path, name string) error {
	// The name comes from /init and lands in a file every resolver reads.
	if !validHostname(name) {
		return fmt.Errorf("invalid hostname %q", name)
	}
	hostsMu.Lock()
	defer hostsMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasSuffix(line, hostnameEntryMarker) {
			if f := strings.Fields(line); len(f) > 1 && f[1] == name {
				return nil
			}
			continue
		}
		if line != "" && mapsName(line, name) {
			return nil
		}
		kept = append(kept, line)
	}
	kept = append(kept, "127.0.1.1\t"+name+"\t"+hostnameEntryMarker)
	body := strings.TrimLeft(strings.Join(kept, "\n"), "\n") + "\n"

	// In place: /etc/hosts is a bind mount, which a rename cannot replace.
	return os.WriteFile(path, []byte(body), 0o644)
}

func mapsName(line, name string) bool {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	for _, f := range fields[min(1, len(fields)):] {
		if f == name {
			return true
		}
	}
	return false
}

// validHostname accepts an RFC 1123 hostname: dot-separated labels of
// letters, digits, and inner hyphens, at most 253 characters.
func validHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

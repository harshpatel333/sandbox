package main

import "testing"

func TestResolveUserTakesImageUserForms(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]struct {
		uid, gid uint32
		home     string
	}{
		"0":               {0, 0, ""},
		"0:0":             {0, 0, ""},
		"root:0":          {0, 0, ""},
		"4242420":         {4242420, 0, "/"},
		"4242420:4242421": {4242420, 4242421, "/"},
	} {
		u, cred, err := resolveUser(spec)
		if err != nil {
			t.Errorf("resolveUser(%q): %v", spec, err)
			continue
		}
		if cred.Uid != want.uid || cred.Gid != want.gid {
			t.Errorf("resolveUser(%q) = %d:%d, want %d:%d", spec, cred.Uid, cred.Gid, want.uid, want.gid)
		}
		if want.home != "" && u.HomeDir != want.home {
			t.Errorf("resolveUser(%q) home = %q, want %q", spec, u.HomeDir, want.home)
		}
	}
	for _, spec := range []string{"no-such-user", "0:no-such-group"} {
		if _, _, err := resolveUser(spec); err == nil {
			t.Errorf("resolveUser(%q) accepted an unknown name", spec)
		}
	}
}

func TestResolveUserKeepsSupplementaryGroups(t *testing.T) {
	t.Parallel()
	_, cred, err := resolveUser("0:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.Groups) == 0 {
		t.Error("root's supplementary groups were dropped")
	}
}

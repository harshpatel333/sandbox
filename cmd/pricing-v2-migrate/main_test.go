package main

import "testing"

func TestApplyRequiresExplicitHumanAuthority(t *testing.T) {
	getenv := func(key string) string {
		if key == "TEAM_CREATION_REGION" {
			return "test-region"
		}
		return ""
	}
	for _, args := range [][]string{
		{"apply"},
		{"apply", "--actor", "operator@example.com", "--reason", "launch"},
		{"apply", "--confirm", "--reason", "launch"},
		{"apply", "--confirm", "--actor", "operator@example.com"},
		{"apply", "--confirm", "--actor", "  ", "--reason", "launch"},
		{"apply", "--confirm", "--actor", "operator@example.com", "--reason", "  "},
	} {
		if _, err := parse(args, getenv); err == nil {
			t.Errorf("accepted apply without authority: %v", args)
		}
	}
	if _, err := parse([]string{"apply", "--confirm", "--actor", "operator@example.com", "--reason", "launch"}, getenv); err != nil {
		t.Fatal(err)
	}
}

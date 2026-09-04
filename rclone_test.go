package main

import (
	"testing"

	"github.com/rclone/rclone/cmd"
)

func TestProjectReleaseVersion(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "release", input: "v1.74.4-pkudisk.1", expected: "v1.74.4-pkudisk.1"},
		{name: "later revision", input: "v1.74.4-pkudisk.12", expected: "v1.74.4-pkudisk.12"},
		{name: "development", input: "(devel)", expected: ""},
		{name: "pseudo version", input: "v0.0.0-20260904000000-deadbeef", expected: ""},
		{name: "upstream", input: "v1.74.4", expected: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := projectReleaseVersion(test.input); got != test.expected {
				t.Fatalf("projectReleaseVersion(%q) = %q, want %q", test.input, got, test.expected)
			}
		})
	}
}

func TestSelfUpdateIsDisabled(t *testing.T) {
	for _, command := range cmd.Root.Commands() {
		if command.Name() == "selfupdate" {
			t.Fatal("upstream selfupdate command is still registered")
		}
	}
}

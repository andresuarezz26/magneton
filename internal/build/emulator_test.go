package build

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSDKRoot(t *testing.T) {
	// $ANDROID_HOME wins when it points at an existing dir.
	dir := t.TempDir()
	t.Setenv("ANDROID_HOME", dir)
	t.Setenv("ANDROID_SDK_ROOT", "")
	if got := detectSDKRoot(); got != dir {
		t.Errorf("ANDROID_HOME: got %q, want %q", got, dir)
	}

	// A non-existent env value is ignored (falls through to the standard paths).
	t.Setenv("ANDROID_HOME", filepath.Join(dir, "does-not-exist"))
	// We can't assert the home-dir fallback deterministically across machines, but
	// it must never return the bogus env path.
	if got := detectSDKRoot(); got == filepath.Join(dir, "does-not-exist") {
		t.Errorf("detectSDKRoot returned a non-existent env path: %q", got)
	}
}

func TestResolvePathsConfiguredRoot(t *testing.T) {
	// A configured root with the expected layout resolves to the SDK binaries.
	root := t.TempDir()
	for _, rel := range []string{"platform-tools/adb", "emulator/emulator"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := ResolvePaths(root)
	if p.ADB != filepath.Join(root, "platform-tools/adb") {
		t.Errorf("ADB = %q", p.ADB)
	}
	if p.Emulator != filepath.Join(root, "emulator/emulator") {
		t.Errorf("Emulator = %q", p.Emulator)
	}
}

func TestParseAVDList(t *testing.T) {
	cases := map[string][]string{
		"Pixel_6_API_34\nPixel_Tablet_API_34\n": {"Pixel_6_API_34", "Pixel_Tablet_API_34"},
		"  Pixel_6  \n\n":                       {"Pixel_6"}, // trims + skips blanks
		"":                                      nil,
		"\n\n":                                  nil,
		"OnlyOne":                               {"OnlyOne"}, // no trailing newline
	}
	for in, want := range cases {
		got := parseAVDList(in)
		if len(got) != len(want) {
			t.Errorf("parseAVDList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseAVDList(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

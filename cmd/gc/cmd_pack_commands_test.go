package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// setupPackCity creates a temp city with a pack that has [[commands]].
// Returns cityPath, packDir.
func setupPackCity(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	cityPath := filepath.Join(dir, "testcity")
	gcDir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	packDir := filepath.Join(dir, "packs", "mypack")
	if err := os.MkdirAll(filepath.Join(packDir, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}

	packTOML := `[pack]
name = "mypack"
schema = 1

[[commands]]
name = "hello"
description = "Say hello"
long_description = "commands/hello-help.txt"
script = "commands/hello.sh"

[[commands]]
name = "info"
description = "Show info"
long_description = "commands/info-help.txt"
script = "commands/info.sh"
`
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(packDir, "commands", "hello-help.txt"),
		[]byte("Say hello to the world.\n\nThis command greets everyone."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "commands", "info-help.txt"),
		[]byte("Show pack info."), 0o644); err != nil {
		t.Fatal(err)
	}

	helloScript := `#!/bin/sh
echo "hello from $GC_PACK_NAME"
`
	if err := os.WriteFile(filepath.Join(packDir, "commands", "hello.sh"), []byte(helloScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "commands", "info.sh"), []byte("#!/bin/sh\necho info output\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cityTOML := `[workspace]
name = "testcity"

[workspace.pack]
path = "` + packDir + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	return cityPath, packDir
}

func TestLoadPackCommandEntries(t *testing.T) {
	_, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	hello := entries[0]
	if hello.PackName != "mypack" {
		t.Errorf("PackName = %q, want %q", hello.PackName, "mypack")
	}
	if hello.Entry.Name != "hello" {
		t.Errorf("Entry.Name = %q, want %q", hello.Entry.Name, "hello")
	}
	if hello.Entry.Description != "Say hello" {
		t.Errorf("Entry.Description = %q, want %q", hello.Entry.Description, "Say hello")
	}
	if hello.Entry.Script != "commands/hello.sh" {
		t.Errorf("Entry.Script = %q, want %q", hello.Entry.Script, "commands/hello.sh")
	}
	if hello.PackDir != packDir {
		t.Errorf("PackDir = %q, want %q", hello.PackDir, packDir)
	}
}

func TestLoadPackCommandEntriesDedup(t *testing.T) {
	_, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir, packDir})
	if len(entries) != 2 {
		t.Fatalf("got %d entries after dedup, want 2", len(entries))
	}
}

func TestLoadPackCommandEntriesBadDir(t *testing.T) {
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{"/nonexistent"})
	if len(entries) != 0 {
		t.Fatalf("got %d entries for nonexistent dir, want 0", len(entries))
	}
}

func TestLoadPackCommandEntriesNilDirs(t *testing.T) {
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, nil)
	if len(entries) != 0 {
		t.Fatalf("got %d entries for nil dirs, want 0", len(entries))
	}
}

func TestRegisterPackCommands_UncachedPacksNoLogNoise(t *testing.T) {
	cityPath := t.TempDir()

	cityTOML := `[workspace]
name = "test"
includes = ["mypk"]

[packs.mypk]
source = "https://example.com/repo.git"
ref = "main"
path = "packs/mypk"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	_, _ = quietLoadCityConfig(cityPath)

	if bytes.Contains(logBuf.Bytes(), []byte("not found, skipping")) {
		t.Fatalf("quietLoadCityConfig produced log noise: %s", logBuf.String())
	}
}

func TestCoreCommandNames(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start", Aliases: []string{"up"}})
	root.AddCommand(&cobra.Command{Use: "stop"})
	root.AddCommand(&cobra.Command{Use: "doctor"})

	names := coreCommandNames(root)
	for _, want := range []string{"start", "up", "stop", "doctor", "help", "completion"} {
		if !names[want] {
			t.Fatalf("core names missing %q", want)
		}
	}
	if names["nonexistent"] {
		t.Fatal("core names should not contain nonexistent")
	}
}

func TestPackCommandTemplateExpansion(t *testing.T) {
	result := expandScriptTemplate("{{.CityRoot}}/bin/run.sh", "/home/user/city", "mytown", "/packs/p1")
	if result != "/home/user/city/bin/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/home/user/city/bin/run.sh")
	}
}

func TestPackCommandTemplateExpansionConfigDir(t *testing.T) {
	result := expandScriptTemplate("{{.ConfigDir}}/scripts/run.sh", "/city", "mytown", "/packs/p1")
	if result != "/packs/p1/scripts/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/packs/p1/scripts/run.sh")
	}
}

func TestPackCommandTemplateNoTemplate(t *testing.T) {
	result := expandScriptTemplate("commands/run.sh", "/city", "mytown", "/packs/p1")
	if result != "commands/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "commands/run.sh")
	}
}

func TestPackCommandTemplateBadTemplate(t *testing.T) {
	result := expandScriptTemplate("{{.Bad", "/city", "mytown", "/packs/p1")
	if result != "{{.Bad" {
		t.Fatalf("expected graceful fallback, got %q", result)
	}
}

func TestNewRootCmdExposesRootPackCommands(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, "commands", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	backstage := findSubcommand(root, "backstage")
	if backstage == nil {
		t.Fatal("missing root pack namespace command")
	}
	if findSubcommand(backstage, "hello") == nil {
		t.Fatal("missing root pack hello command")
	}
}

// TestShouldDiscoverPackCommands pins the gate that keeps the ~1s pack-command
// discovery (city resolution + config load) off the hot path. When argv targets
// a concrete built-in command, discovery must be skipped; for a bare invocation,
// help, completion, or an unknown (possibly-pack) first token, it must run.
func TestShouldDiscoverPackCommands(t *testing.T) {
	root := &cobra.Command{Use: "gc", RunE: func(*cobra.Command, []string) error { return nil }}
	root.PersistentFlags().String("city", "", "")
	root.AddCommand(&cobra.Command{Use: "version", RunE: func(*cobra.Command, []string) error { return nil }})
	events := &cobra.Command{Use: "events", RunE: func(*cobra.Command, []string) error { return nil }}
	events.Flags().Bool("watch", false, "")
	root.AddCommand(events)

	skip := [][]string{
		{"version"},
		{"events"},
		{"events", "--watch"},
		{"--city", "/somewhere", "events"}, // global flag before a built-in
	}
	for _, argv := range skip {
		if shouldDiscoverPackCommands(root, argv) {
			t.Errorf("argv %v targets a built-in; want skip (false), got discover (true)", argv)
		}
	}

	discover := [][]string{
		{},                  // bare gc -> help lists all commands
		{"--help"},          // root help must list pack commands
		{"help"},            // help subcommand (added lazily by cobra) not yet a built-in
		{"completion"},      // shell completion must list pack commands
		{"somepackcommand"}, // unknown first token might be a pack command
	}
	for _, argv := range discover {
		if !shouldDiscoverPackCommands(root, argv) {
			t.Errorf("argv %v has no built-in target; want discover (true), got skip (false)", argv)
		}
	}
}

// TestNewRootCmdGatesPackDiscoveryByArgv is the behavioral regression pin: with
// a real pack city on the cwd, building the root for a built-in command
// (`version`) must NOT register the pack namespace — proving the expensive
// discovery I/O was skipped — while a bare or unknown-command build still does.
func TestNewRootCmdGatesPackDiscoveryByArgv(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, "commands", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// Built-in target: discovery must be skipped, pack namespace absent.
	builtin := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{}, "version")
	if findSubcommand(builtin, "backstage") != nil {
		t.Fatal("pack discovery ran for a built-in command (`version`); it must be skipped")
	}

	// Bare invocation: help lists everything, so discovery must run.
	bare := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	if findSubcommand(bare, "backstage") == nil {
		t.Fatal("pack discovery skipped for a bare invocation; help must list pack commands")
	}

	// Unknown first token (might be a pack command): discovery must run.
	unknown := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{}, "backstage")
	if findSubcommand(unknown, "backstage") == nil {
		t.Fatal("pack discovery skipped for an unknown command; pack commands must be discoverable")
	}
}

func TestLegacyPackCommandHelpFlagUsesBuiltInHelp(t *testing.T) {
	cityPath, packDir := setupPackCity(t)

	root := &cobra.Command{Use: "gc"}
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})

	var stdout, stderr bytes.Buffer
	addPackCommandsToRoot(root, entries, cityPath, "testcity", &stdout, &stderr)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"mypack", "hello", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Say hello to the world.") {
		t.Fatalf("stdout missing long help, got:\n%s", out)
	}
	if strings.Contains(out, "hello from mypack") {
		t.Fatalf("help should not execute the pack command, got:\n%s", out)
	}
}

func TestSetupPackCityWritesExpectedLayout(t *testing.T) {
	cityPath, packDir := setupPackCity(t)
	for _, path := range []string{
		filepath.Join(cityPath, "city.toml"),
		filepath.Join(packDir, "pack.toml"),
		filepath.Join(packDir, "commands", "hello.sh"),
		filepath.Join(packDir, "commands", "info.sh"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist: %v", path, err)
		}
	}
	if !strings.Contains(cityPath, "testcity") {
		t.Fatalf("cityPath = %q, want testcity suffix", cityPath)
	}
}

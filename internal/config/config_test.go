package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture copies independent test data so local deployment edits cannot affect tests.
func fixture(t *testing.T, edit func(string) string) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"config.yaml", "dailybuild-artifacts.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "config.yaml" && edit != nil {
			data = []byte(edit(string(data)))
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(directory, "config.yaml")
}

func TestLoadSampleConfig(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config", "config.sample.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFixtureConfig(t *testing.T) {
	filename := fixture(t, nil)
	cfg, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paths.ReleasesRoot != filepath.Join(filepath.Dir(filename), "releases") {
		t.Fatal("relative root was not resolved beside YAML")
	}
	if len(cfg.Manifest.Artifacts) != 3 {
		t.Fatalf("artifact count = %d", len(cfg.Manifest.Artifacts))
	}
	if cfg.Sources.Dailybuild.GitHub.Workflow != "build.yml" {
		t.Fatal("workflow filename must remain configurable")
	}
	for _, artifact := range cfg.Manifest.Artifacts {
		wantRequired := artifact.Sources["rva23"].Type == "static"
		if *artifact.Required != wantRequired {
			t.Fatalf("unexpected required flag for %s", artifact.ID)
		}
	}
	url, err := cfg.SourceURL(cfg.Manifest.Artifacts[0].Sources["rva23"])
	if err != nil || url != "https://images.example.org/build:/rva23/rootfs.tar" {
		t.Fatalf("URL = %q: %v", url, err)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(string) string{
		"unknown field":      func(s string) string { return s + "\nunknown_field: true\n" },
		"duplicate key":      func(s string) string { return s + "\nversion: 1\n" },
		"multiple documents": func(s string) string { return s + "\n---\nversion: 1\n" },
		"missing root":       func(s string) string { return strings.Replace(s, "repo_root: repo", "repo_root: ''", 1) },
		"overlap":            func(s string) string { return strings.Replace(s, "repo_root: repo", "repo_root: releases/nested", 1) },
		"published staging": func(s string) string {
			return strings.Replace(s, "staging_root: releases/.staging", "staging_root: releases/rolling/.staging", 1)
		},
		"unsupported target": func(s string) string { return strings.Replace(s, "  x86_64:", "  unsupported:", 1) },
		"invalid timezone":   func(s string) string { return strings.Replace(s, "UTC+08:00", "Local", 1) },
		"unsafe layout":      func(s string) string { return strings.Replace(s, "rolling: rolling", "rolling: ../rolling", 1) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fixture(t, edit)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestTargetSwitchesAndSelection(t *testing.T) {
	cfg, err := Load(fixture(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.Targets["rva20"]
	target.Repo.Enabled = false
	cfg.Targets["rva20"] = target
	if _, err := cfg.SelectTargets([]string{"rva20"}, "repo"); err == nil {
		t.Fatal("disabled repo selected")
	}
	if _, err := cfg.SelectTargets([]string{"rva20"}, "dailybuild"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SelectTargets([]string{"x86_64"}, "dailybuild"); err == nil {
		t.Fatal("x86_64 dailybuild selected")
	}
	for _, requested := range [][]string{{"unknown"}, {""}, {"rva23", "rva23"}} {
		if _, err := cfg.SelectTargets(requested, "repo"); err == nil {
			t.Fatalf("accepted invalid selection: %v", requested)
		}
	}
	target.Enabled = false
	cfg.Targets["rva20"] = target
	if _, err := cfg.SelectTargets([]string{"rva20"}, "dailybuild"); err == nil {
		t.Fatal("globally disabled target selected")
	}
}

func TestDatesPathsAndTimezone(t *testing.T) {
	zone, err := ParseTimezone("UTC+08:00")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Location: zone}
	date, err := cfg.Date("", time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC))
	if err != nil || date != "2026-11-12" {
		t.Fatalf("date = %s: %v", date, err)
	}
	for _, date := range []string{"2026-02-29", "0000-01-01", "2026-1-01", "../2026-11-12"} {
		if ValidDate(date) {
			t.Fatalf("accepted %s", date)
		}
	}
	for _, relative := range []string{"../x", "/x", "x/../y", "x\\y", "x?query"} {
		if _, err := JoinURL("https://example.org/", relative); err == nil {
			t.Fatalf("accepted URL path %s", relative)
		}
	}
	for _, name := range []string{"../x", "index.html", "SHA256SUMS", "{unknown}"} {
		if _, err := (Artifact{Filename: name}).RenderFilename("2026-11-12", "rva23"); err == nil {
			t.Fatalf("accepted filename %s", name)
		}
	}
	if _, err := ParseTimezone("Asia/Taipei"); err != nil {
		t.Fatal(err)
	}
}

func TestManifestValidation(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Manifest.Artifacts[0].Required = nil },
		func(c *Config) { c.Manifest.Artifacts[1].Filename = c.Manifest.Artifacts[0].Filename },
		func(c *Config) {
			c.Manifest.Artifacts[0].Sources["rva23"] = ArtifactSource{Type: "github", Match: "../*"}
		},
		func(c *Config) {
			c.Manifest.Artifacts[0].Sources["rva23"] = ArtifactSource{Type: "rpm", Package: "ovmf", Architecture: "riscv64", InternalPath: "../file"}
		},
	} {
		cfg, err := Load(fixture(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		change(cfg)
		if err := cfg.validate(); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
}

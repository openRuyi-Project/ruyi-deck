package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
)

func repoConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "testdata", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg.Paths.RepoRoot = filepath.Join(root, "repo")
	cfg.Paths.ReleasesRoot = filepath.Join(root, "releases")
	cfg.Paths.ArchiveRoot = filepath.Join(root, "archive")
	cfg.Paths.StagingRoot = filepath.Join(root, "staging")
	return cfg
}

func metadata(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, "repodata"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "repodata", "repomd.xml"), []byte("<repomd></repomd>"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncMappingAndDelete(t *testing.T) {
	cfg := repoConfig(t)
	options := Options{Targets: []string{"rva23"}}
	p, err := prepare(cfg, options, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Operations) != 1 || p.Operations[0].Source != "https://repo.example.org/rolling/rva23/riscv64" {
		t.Fatalf("plan = %+v", p)
	}
	if strings.Contains(strings.Join(arguments(p.Operations[0], p.Root), " "), "--delete") {
		t.Fatal("first sync must not delete")
	}
	if strings.Contains(strings.Join(arguments(p.Operations[0], p.Root), " "), "--disablerepo") {
		t.Fatal("DNF 4 --repoid must not be combined with --disablerepo")
	}
	metadata(t, p.Operations[0].Destination)
	p, err = prepare(cfg, options, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Operations[0].Incremental || !strings.Contains(strings.Join(arguments(p.Operations[0], p.Root), " "), "--delete") {
		t.Fatal("valid repo must use incremental deletion")
	}
	if _, err := prepare(cfg, Options{Targets: []string{"rva20"}}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRepoAndArchiveConflict(t *testing.T) {
	cfg := repoConfig(t)
	directory := filepath.Join(cfg.Paths.RepoRoot, "rolling", "rva20")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unexpected"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(cfg, Options{Targets: []string{"rva20"}}, false, time.Now()); err == nil {
		t.Fatal("invalid nonempty repo accepted")
	}
	metadata(t, directory)
	p, err := prepare(cfg, Options{Targets: []string{"rva20"}, Date: "2026-11-12"}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := p.Operations[0]
	if op.Destination != filepath.Join(cfg.Paths.ArchiveRoot, "repo", "rolling", "2026", "11", "12", "rva20") {
		t.Fatalf("destination = %s", op.Destination)
	}
	if op.Source != "file://"+directory || op.Incremental {
		t.Fatalf("archive operation = %+v", op)
	}
	if err := os.MkdirAll(op.Destination, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(cfg, Options{Targets: []string{"rva20"}, Date: "2026-11-12"}, true, time.Now()); err == nil {
		t.Fatal("existing archive accepted")
	}
}

func TestDryRunAndPluginFailureDoNotWrite(t *testing.T) {
	cfg := repoConfig(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "dnf"), []byte("fake executable; never run"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	calls := 0
	run := func(context.Context, io.Writer, string, ...string) error {
		calls++
		return errors.New("plugin unavailable")
	}
	var out bytes.Buffer
	options := Options{Targets: []string{"rva20"}, DryRun: true}
	if err := execute(context.Background(), cfg, options, false, &out, run); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dry-run invoked DNF")
	}
	if _, err := os.Stat(cfg.Paths.RepoRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run wrote root: %v", err)
	}
	options.DryRun = false
	if err := execute(context.Background(), cfg, options, false, io.Discard, run); err == nil {
		t.Fatal("missing plugin accepted")
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	if _, err := os.Stat(cfg.Paths.RepoRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plugin precheck wrote root")
	}
}

func TestFirstSyncPublishesOnlyValidatedRepo(t *testing.T) {
	cfg := repoConfig(t)
	p, err := prepare(cfg, Options{Targets: []string{"rva23"}}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := p.Operations[0]
	var observed []string
	run := func(_ context.Context, _ io.Writer, name string, args ...string) error {
		if name != "dnf" {
			t.Fatalf("program = %s", name)
		}
		observed = append([]string(nil), args...)
		for _, argument := range args {
			if strings.HasPrefix(argument, "--download-path=") {
				metadata(t, filepath.Join(strings.TrimPrefix(argument, "--download-path="), "rva23"))
			}
		}
		return nil
	}
	if err := perform(context.Background(), io.Discard, op, p, run); err != nil {
		t.Fatal(err)
	}
	if err := validMetadata(op.Destination); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(observed, arguments(op, p.Root)) {
		t.Fatal("initial sync wrote directly to published parent")
	}
	if err := perform(context.Background(), io.Discard, op, p, func(context.Context, io.Writer, string, ...string) error { return errors.New("network failed") }); err == nil {
		t.Fatal("external error ignored")
	}
	if err := validMetadata(op.Destination); err != nil {
		t.Fatal("failure damaged successful repo")
	}
}

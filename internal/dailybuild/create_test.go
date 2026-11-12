package dailybuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
)

func dailyConfig(t *testing.T, address string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "testdata", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg.Paths.RepoRoot = filepath.Join(root, "repo")
	cfg.Paths.ReleasesRoot = filepath.Join(root, "releases")
	cfg.Paths.ArchiveRoot = filepath.Join(root, "archive")
	cfg.Paths.StagingRoot = filepath.Join(cfg.Paths.ReleasesRoot, ".staging")
	required, optional := true, false
	cfg.Manifest.Artifacts = []config.Artifact{
		{ID: "image", Filename: "image-{date}.img", Description: "Required image", Required: &required, Sources: map[string]config.ArtifactSource{
			"rva20": {Type: "static", URL: address + "/rva20"}, "rva23": {Type: "static", URL: address + "/rva23"},
		}},
		{ID: "extra", Filename: "optional.img", Description: "Optional <script>alert(1)</script>", Required: &optional, Sources: map[string]config.ArtifactSource{
			"rva20": {Type: "static", URL: address + "/missing"}, "rva23": {Type: "static", URL: address + "/missing"},
		}},
	}
	return cfg
}

func readText(t *testing.T, filename string) string {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIndependentPublicationAndSameDateSupplement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" || r.URL.Path == "/rva20" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "image")
	}))
	defer server.Close()
	cfg := dailyConfig(t, server.URL)
	date := "2026-11-12"
	root := filepath.Join(cfg.Paths.ReleasesRoot, "rolling")
	var output bytes.Buffer
	if err := Create(context.Background(), cfg, Options{Date: date}, &output); err == nil {
		t.Fatal("partial publication must report a failed target")
	}
	if current, err := readCurrent(root, "current"); err != nil || current != date {
		t.Fatalf("current = %s: %v", current, err)
	}
	if _, err := os.Stat(filepath.Join(root, date, "rva20")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete target was published")
	}
	index := filepath.Join(root, date, "rva23", "index.html")
	page := readText(t, index)
	if !strings.Contains(output.String(), "Warning: Dailybuild "+date+" is incomplete.\nMissing targets: rva20") {
		t.Fatalf("missing warning: %s", output.String())
	}
	if !strings.Contains(page, "Missing targets: rva20") || !strings.Contains(page, `<td class="missing">optional.img</td><td></td><td></td>`) {
		t.Fatal("missing target/file display is incorrect")
	}
	if strings.Contains(page, `href="optional.img"`) || strings.Contains(page, "<script>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("missing-file link or unsafe HTML")
	}
	sumsFile := filepath.Join(root, date, "rva23", "SHA256SUMS")
	sums := readText(t, sumsFile)
	want := fmt.Sprintf("%x  image-%s.img\n", sha256.Sum256([]byte("image")), date)
	if sums != want {
		t.Fatalf("SHA256SUMS = %q; want %q", sums, want)
	}
	cfg.Manifest.Artifacts[0].Sources["rva20"] = config.ArtifactSource{Type: "static", URL: server.URL + "/ok"}
	if err := Create(context.Background(), cfg, Options{Targets: []string{"rva20"}, Date: date}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readText(t, index), "incomplete") {
		t.Fatal("same-date supplement did not refresh the earlier index")
	}
	if readText(t, sumsFile) != sums {
		t.Fatal("supplement modified existing checksums")
	}
	if err := Create(context.Background(), cfg, Options{Targets: []string{"rva23"}, Date: date}, io.Discard); err == nil {
		t.Fatal("existing target was overwritten")
	}
	entries, err := os.ReadDir(cfg.Paths.StagingRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging leaked: %v: %v", entries, err)
	}
}

func TestAllFailedPreservesCurrentAndSkipsCleanup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failed", http.StatusBadGateway) }))
	defer server.Close()
	cfg := dailyConfig(t, server.URL)
	root := filepath.Join(cfg.Paths.ReleasesRoot, "rolling")
	for _, date := range []string{"2026-11-10", "2001-01-01"} {
		if err := os.MkdirAll(filepath.Join(root, date), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := updateCurrent(root, "current", "2026-11-10"); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), cfg, Options{Date: "2026-11-12"}, io.Discard); err == nil {
		t.Fatal("all failures reported success")
	}
	if current, err := readCurrent(root, "current"); err != nil || current != "2026-11-10" {
		t.Fatalf("current changed: %s: %v", current, err)
	}
	if _, err := os.Stat(filepath.Join(root, "2001-01-01")); err != nil {
		t.Fatal("failed release triggered cleanup")
	}
	if _, err := os.Stat(filepath.Join(root, "2026-11-12")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty release date became public")
	}
}

func TestDryRunHasNoNetworkOrFilesystemChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("dry-run sent HTTP request") }))
	defer server.Close()
	cfg := dailyConfig(t, server.URL)
	var output bytes.Buffer
	if err := Create(context.Background(), cfg, Options{DryRun: true, Targets: []string{"rva23"}, Date: "2026-11-12"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), server.URL+"/rva23") {
		t.Fatal("dry-run omitted source")
	}
	if _, err := os.Stat(cfg.Paths.ReleasesRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry-run wrote release root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Create(ctx, cfg, Options{}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption = %v", err)
	}
}

func TestUnexpectedDateContentsRejected(t *testing.T) {
	cfg := dailyConfig(t, "https://example.org")
	directory := filepath.Join(cfg.Paths.ReleasesRoot, "rolling", "2026-11-12")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unexpected"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(cfg, Options{Targets: []string{"rva23"}, Date: "2026-11-12"}, time.Now()); err == nil {
		t.Fatal("unexpected content accepted")
	}
}

func TestCurrentNeverMovesBackwardAndRejectsOrdinaryFile(t *testing.T) {
	root := t.TempDir()
	for _, date := range []string{"2026-11-11", "2026-11-12"} {
		if err := os.Mkdir(filepath.Join(root, date), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := updateCurrent(root, "current", "2026-11-12"); err != nil {
		t.Fatal(err)
	}
	if err := updateCurrent(root, "current", "2026-11-11"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(root, "current")); err != nil || got != "2026-11-12" {
		t.Fatalf("current = %q: %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := updateCurrent(root, "ordinary", "2026-11-12"); err == nil {
		t.Fatal("ordinary file replaced by link")
	}
	if readText(t, filepath.Join(root, "ordinary")) != "keep" {
		t.Fatal("ordinary file was modified")
	}
}

func TestRetentionUsesSevenCalendarDaysAndIgnoresSymlinks(t *testing.T) {
	cfg := dailyConfig(t, "https://example.org")
	root := t.TempDir()
	for _, date := range []string{"2026-11-04", "2026-11-05", "2026-11-06", "2026-11-12", "not-a-date"} {
		if err := os.Mkdir(filepath.Join(root, date), 0755); err != nil {
			t.Fatal(err)
		}
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "2026-11-03")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 0, 1, 0, 0, cfg.Location)
	expired, err := expiredDirectories(root, now, cfg, "2026-11-04", "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expired, []string{filepath.Join(root, "2026-11-05")}) {
		t.Fatalf("expired = %v", expired)
	}
	if err := updateCurrent(root, "current", "2026-11-04"); err != nil {
		t.Fatal(err)
	}
	if err := cleanupExpired(context.Background(), io.Discard, cfg, &plan{Root: root, Date: "2026-11-12"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026-11-05")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired release was not removed")
	}
	if info, err := os.Lstat(filepath.Join(root, "2026-11-03")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("cleanup touched a symlink")
	}
}

func TestRPMDependenciesCheckedBeforeWrites(t *testing.T) {
	cfg := dailyConfig(t, "https://example.org")
	cfg.Manifest.Artifacts[1].Sources["rva23"] = config.ArtifactSource{Type: "rpm", Package: "ovmf", Architecture: "riscv64", InternalPath: "usr/share/ovmf/virt_code.fd"}
	t.Setenv("PATH", t.TempDir())
	err := Create(context.Background(), cfg, Options{Targets: []string{"rva23"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rpm2cpio") {
		t.Fatalf("missing dependency error = %v", err)
	}
	if _, err := os.Stat(cfg.Paths.ReleasesRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dependency check created release directory")
	}
}

func TestMissingGitHubTokenKeepsOptionalFilename(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "image") }))
	defer server.Close()
	cfg := dailyConfig(t, server.URL)
	cfg.Manifest.Artifacts[1].Sources["rva23"] = config.ArtifactSource{Type: "github", Match: "example-rootfs.cpio.gz"}
	t.Setenv(cfg.Sources.Dailybuild.GitHub.TokenEnv, "")
	var out bytes.Buffer
	if err := Create(context.Background(), cfg, Options{Targets: []string{"rva23"}, Date: "2026-11-12"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GITHUB_TOKEN is not set") {
		t.Fatal("missing token was not explained")
	}
	page := readText(t, filepath.Join(cfg.Paths.ReleasesRoot, "rolling", "2026-11-12", "rva23", "index.html"))
	if !strings.Contains(page, `<td class="missing">optional.img</td>`) {
		t.Fatal("missing optional Zero entry was omitted")
	}
}

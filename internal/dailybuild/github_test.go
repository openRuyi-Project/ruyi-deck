package dailybuild

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
)

func TestGitHubLatestSuccessAcrossPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing token")
		}
		if strings.Contains(r.URL.Path, "/artifacts") {
			io.WriteString(w, `{"artifacts":[{"id":10,"name":"output","expired":true},{"id":11,"name":"output","expired":false}]}`)
			return
		}
		if r.URL.Query().Get("status") != "success" {
			t.Error("request must filter successful runs")
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/ignored>; rel="next"`)
			io.WriteString(w, `{"workflow_runs":[{"id":99,"status":"completed","conclusion":"failure","updated_at":"2026-11-14T00:00:00Z"},{"id":2,"status":"completed","conclusion":"success","updated_at":"2026-11-10T00:00:00Z"}]}`)
		} else {
			io.WriteString(w, `{"workflow_runs":[{"id":3,"status":"in_progress","conclusion":"success","updated_at":"2026-11-13T00:00:00Z"},{"id":1,"status":"completed","conclusion":"success","updated_at":"2026-11-12T00:00:00Z"}]}`)
		}
	}))
	defer server.Close()
	g := githubAPI{client: server.Client(), base: server.URL, token: "secret", settings: config.GitHubSource{Owner: "owner", Repository: "repo", Workflow: "build.yml", ArtifactName: "output"}}
	run, err := g.latestRun(context.Background())
	if err != nil || run.ID != 1 {
		t.Fatalf("run = %+v: %v", run, err)
	}
	artifact, err := g.artifact(context.Background(), run.ID)
	if err != nil || artifact.ID != 11 {
		t.Fatalf("artifact = %+v: %v", artifact, err)
	}
	g.token = ""
	if err := g.downloadZIP(context.Background(), filepath.Join(t.TempDir(), "none.zip")); err == nil {
		t.Fatal("missing token accepted")
	}
}

func makeZIP(t *testing.T, names ...string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "output.zip")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(entry, "contents of %s", name); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestZIPVersionPatternsAndSafety(t *testing.T) {
	for _, version := range []string{"7.2.6-1.0_555.1.or", "8.0-99.or"} {
		archive := makeZIP(t, "nested/openruyi-linux-core-vmlinuz-"+version, "openruyi-linux-lts-core-vmlinuz-6.18")
		target := filepath.Join(t.TempDir(), "zero.kernel")
		if err := extractZIP(context.Background(), archive, "openruyi-linux-core-vmlinuz-*", target); err != nil {
			t.Fatal(err)
		}
		if err := checkFile(target); err != nil {
			t.Fatal(err)
		}
	}
	for _, names := range [][]string{{"unrelated"}, {"kernel-1", "kernel-2"}, {"../kernel-1"}, {"/kernel-1"}, {"dir\\kernel-1"}} {
		if err := extractZIP(context.Background(), makeZIP(t, names...), "kernel-*", filepath.Join(t.TempDir(), "kernel")); err == nil {
			t.Fatalf("unsafe or ambiguous ZIP accepted: %v", names)
		}
	}
}

func TestGitHubErrorsNeverPrintResponseSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "secret-in-body")
	}))
	defer server.Close()
	g := githubAPI{client: server.Client(), base: server.URL, token: "secret-token"}
	_, err := g.latestRun(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-in-body") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestZIPRejectsSymbolicLinks(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "unsafe.zip")
	out, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(out)
	header := &zip.FileHeader{Name: "kernel-1"}
	header.SetMode(os.ModeSymlink | 0777)
	member, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(member, "/etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractZIP(context.Background(), filename, "kernel-*", filepath.Join(t.TempDir(), "kernel")); err == nil {
		t.Fatal("ZIP symlink accepted")
	}
}

func TestGitHubDownloadsArtifactFromSelectedRun(t *testing.T) {
	archive := makeZIP(t, "openruyi-base.cpio.gz")
	archiveBytes, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("authenticated endpoint did not receive token")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs"):
			io.WriteString(w, `{"workflow_runs":[{"id":42,"status":"completed","conclusion":"success","updated_at":"2026-11-12T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/runs/42/artifacts"):
			io.WriteString(w, `{"artifacts":[{"id":7,"name":"output","expired":false}]}`)
		case strings.HasSuffix(r.URL.Path, "/artifacts/7/zip"):
			w.Write(archiveBytes)
		default:
			t.Errorf("unexpected API request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := githubAPI{client: server.Client(), base: server.URL, token: "secret", settings: config.GitHubSource{Owner: "owner", Repository: "repo", Workflow: "build.yml", ArtifactName: "output"}}
	destination := filepath.Join(t.TempDir(), "github.zip")
	if err := g.downloadZIP(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	if string(archiveBytes) != readText(t, destination) {
		t.Fatal("downloaded artifact differs from API content")
	}
}

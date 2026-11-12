package dailybuild

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadStatusEmptyTruncationAndRename(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Last-Modified", "Thu, 12 Nov 2026 01:02:03 GMT")
			io.WriteString(w, "image-content")
		case "/missing":
			http.NotFound(w, r)
		case "/empty":
			w.WriteHeader(http.StatusOK)
		case "/truncated":
			w.Header().Set("Content-Length", "100")
			io.WriteString(w, "short")
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	for _, endpoint := range []string{"ok", "missing", "empty", "truncated", "error"} {
		t.Run(endpoint, func(t *testing.T) {
			directory := t.TempDir()
			target := filepath.Join(directory, "renamed.img")
			err := download(context.Background(), server.Client(), server.URL+"/"+endpoint, target)
			if endpoint == "ok" {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "image-content" {
					t.Fatalf("download = %q: %v", data, err)
				}
				info, err := os.Stat(target)
				if err != nil || info.ModTime().UTC().Format("15:04:05") != "01:02:03" {
					t.Fatalf("modification time: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("invalid response accepted")
				}
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial download was published")
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".part") {
					t.Fatal("partial file leaked")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := download(ctx, server.Client(), server.URL+"/ok", filepath.Join(t.TempDir(), "cancel.img")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestRedirectStripsAuthorization(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
		io.WriteString(w, "zip")
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("origin did not receive authorization")
		}
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-secret")
	if err := downloadRequest(context.Background(), client, request, filepath.Join(t.TempDir(), "archive.zip")); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("token leaked to redirect host")
	}
}

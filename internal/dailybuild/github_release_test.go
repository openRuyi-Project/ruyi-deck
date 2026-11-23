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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
)

// releaseTestTransport routes the production API origin to a local test server.
type releaseTestTransport struct {
	address *url.URL
	next    http.RoundTripper
}

func (t releaseTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	local := request.Clone(request.Context())
	local.URL.Scheme = t.address.Scheme
	local.URL.Host = t.address.Host
	return t.next.RoundTrip(local)
}

func testReleaseStore(t *testing.T, server *httptest.Server, out io.Writer) *sourceStore {
	t.Helper()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := newHTTPClient()
	client.Transport = releaseTestTransport{address: address, next: client.Transport}
	t.Cleanup(client.CloseIdleConnections)
	cfg := dailyConfig(t, server.URL)
	cfg.Sources.Dailybuild.GitHubRelease.Token = ""
	return &sourceStore{cfg: cfg, client: client, directory: t.TempDir(), out: out}
}

func TestReleaseDiscoveryPinsLatestAndReadsEveryAssetPage(t *testing.T) {
	latestCalls, assetPages := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public release request unexpectedly included a token")
		}
		switch r.URL.Path {
		case "/repos/example-owner/example-images/releases/latest":
			latestCalls++
			// A second lookup would switch to another release mid-publication.
			if latestCalls > 1 {
				io.WriteString(w, `{"id":99,"tag_name":"v2"}`)
			} else {
				io.WriteString(w, `{"id":42,"tag_name":"v1"}`)
			}
		case "/repos/example-owner/example-images/releases/42/assets":
			assetPages++
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<https://api.github.com/ignored>; rel="next"`)
				io.WriteString(w, `[{"id":11,"name":"openRuyi-installiso.riscv64-2026.11.23.iso","state":"uploaded","size":3},{"id":13,"name":"openRuyi-installiso.riscv64-2026.11.23.iso.sha256","state":"uploaded","size":100}]`)
			} else {
				io.WriteString(w, `[{"id":12,"name":"openRuyi-installiso.riscv64-2026.11.23.qcow2","state":"uploaded","size":5}]`)
			}
		case "/repos/example-owner/example-images/releases/assets/11":
			io.WriteString(w, "iso")
		case "/repos/example-owner/example-images/releases/assets/12":
			io.WriteString(w, "qcow2")
		default:
			t.Errorf("unexpected API request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	store := testReleaseStore(t, server, &output)
	for _, extension := range []string{"iso", "qcow2"} {
		artifact := artifactPlan{Source: config.ArtifactSource{Type: "github_release", Match: "openRuyi-installiso.riscv64-*." + extension}}
		destination := filepath.Join(store.directory, "renamed."+extension)
		if err := store.fetch(context.Background(), "rva23", artifact, destination); err != nil {
			t.Fatal(err)
		}
		if got := readText(t, destination); got != extension {
			t.Fatalf("asset contents = %q", got)
		}
	}
	if latestCalls != 1 || assetPages != 2 {
		t.Fatalf("latest lookups = %d; asset pages = %d", latestCalls, assetPages)
	}
	if !strings.Contains(output.String(), `tag "v1"`) || !strings.Contains(output.String(), "openRuyi-installiso.riscv64-2026.11.23.iso") {
		t.Fatal("output omitted the selected tag or original asset name")
	}
}

func TestReleaseAssetFilenameMatching(t *testing.T) {
	cases := []struct {
		name    string
		names   []string
		pattern string
		want    string
	}{
		{"date and target", []string{"image-rva23-2026.11.23.iso", "image-rva20-2026.11.23.iso", "image-rva23-2026.11.23.iso.sha256"}, "image-rva23-*.iso", "image-rva23-2026.11.23.iso"},
		{"date changed", []string{"image-rva23-2027.01.01.iso"}, "image-rva23-*.iso", "image-rva23-2027.01.01.iso"},
		{"missing", []string{"image-rva20-2026.11.23.iso"}, "image-rva23-*.iso", ""},
		{"ambiguous dates", []string{"image-rva23-2026.11.22.iso", "image-rva23-2026.11.23.iso"}, "image-rva23-*.iso", ""},
		{"invalid pattern", []string{"image.iso"}, "[", ""},
		{"unsafe name", []string{"image\n.iso"}, "*.iso", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var assets []githubReleaseAsset
			for i, name := range tc.names {
				assets = append(assets, githubReleaseAsset{ID: int64(i + 1), Name: name, State: "uploaded", Size: 1})
			}
			asset, err := matchReleaseAsset(assets, tc.pattern)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid or ambiguous asset selection accepted")
				}
			} else if err != nil || asset.Name != tc.want {
				t.Fatalf("selected asset = %q: %v", asset.Name, err)
			}
		})
	}
	for _, asset := range []githubReleaseAsset{
		{ID: 0, Name: "image.iso", State: "uploaded", Size: 1},
		{ID: 1, Name: "image.iso", State: "starter", Size: 1},
		{ID: 1, Name: "image.iso", State: "uploaded", Size: 0},
	} {
		if _, err := matchReleaseAsset([]githubReleaseAsset{asset}, "*.iso"); err == nil {
			t.Fatalf("incomplete asset accepted: %+v", asset)
		}
	}
}

func TestReleaseAssetDownloadChecksSizeDigestAndAuthentication(t *testing.T) {
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("image")))
	cases := []struct {
		name        string
		size        int64
		digest      string
		token       string
		status      int
		wantError   bool
		wantRequest bool
	}{
		{"public without digest", 5, "", "", 200, false, true},
		{"authenticated with digest", 5, digest, "test-secret", 200, false, true},
		{"wrong size", 6, digest, "", 200, true, true},
		{"wrong digest", 5, "sha256:" + strings.Repeat("0", 64), "", 200, true, true},
		{"malformed digest", 5, "sha256:xyz", "", 200, true, false},
		{"unsupported digest", 5, "other:abc", "", 200, true, false},
		{"HTTP error", 5, "", "test-secret", 403, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				wantAuthorization := ""
				if tc.token != "" {
					wantAuthorization = "Bearer " + tc.token
				}
				if r.Header.Get("Authorization") != wantAuthorization || r.Header.Get("Accept") != "application/octet-stream" {
					t.Error("incorrect asset request authentication or media type")
				}
				if r.URL.Path != "/repos/owner/images/releases/assets/7" {
					t.Errorf("unexpected asset endpoint %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				if tc.status != http.StatusOK {
					io.WriteString(w, "secret-in-response")
				} else {
					io.WriteString(w, "image")
				}
			}))
			defer server.Close()
			g := githubAPI{client: server.Client(), base: server.URL, token: tc.token, settings: config.GitHubSource{Owner: "owner", Repository: "images"}}
			modified := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
			asset := githubReleaseAsset{ID: 7, Name: "image.iso", State: "uploaded", Size: tc.size, Digest: tc.digest, UpdatedAt: modified}
			directory := t.TempDir()
			target := filepath.Join(directory, "renamed.iso")
			err := g.downloadReleaseAsset(context.Background(), asset, target)
			if (err != nil) != tc.wantError || (requests > 0) != tc.wantRequest {
				t.Fatalf("download error = %v; requests = %d", err, requests)
			}
			if err != nil {
				if strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "secret-in-response") {
					t.Fatal("download error leaked a secret")
				}
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 0 {
					t.Fatalf("failed download left files: %v: %v", entries, err)
				}
			} else {
				if readText(t, target) != "image" {
					t.Fatal("incorrect asset contents")
				}
				info, err := os.Stat(target)
				if err != nil || !info.ModTime().Equal(modified) {
					t.Fatalf("asset modification time: %v", err)
				}
			}
		})
	}
}

func TestReleaseLookupFailureIsCached(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "secret-in-response")
	}))
	defer server.Close()
	store := testReleaseStore(t, server, io.Discard)
	for _, pattern := range []string{"*.iso", "*.qcow2"} {
		if err := store.fetchRelease(context.Background(), pattern, filepath.Join(store.directory, "unused")); err == nil || strings.Contains(err.Error(), "secret-in-response") {
			t.Fatalf("lookup error = %v", err)
		}
	}
	if requests != 1 {
		t.Fatalf("failed latest lookup was repeated %d times", requests)
	}
}

func TestReleaseAssetFailureRespectsRequiredFlag(t *testing.T) {
	for _, imageCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("matching-images-%d", imageCount), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/latest"):
					io.WriteString(w, `{"id":42,"tag_name":"v1"}`)
				case strings.HasSuffix(r.URL.Path, "/42/assets"):
					if imageCount == 0 {
						io.WriteString(w, `[]`)
					} else if imageCount == 1 {
						io.WriteString(w, `[{"id":7,"name":"image-2026.11.23.iso","state":"uploaded","size":5}]`)
					} else {
						io.WriteString(w, `[{"id":7,"name":"image-2026.11.23.iso","state":"uploaded","size":5},{"id":8,"name":"image-2026.11.22.iso","state":"uploaded","size":5}]`)
					}
				case strings.HasSuffix(r.URL.Path, "/assets/7"):
					io.WriteString(w, "image")
				default:
					t.Errorf("unexpected API request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			store := testReleaseStore(t, server, &output)
			cfg := store.cfg
			cfg.Manifest.Artifacts[0].Sources["rva23"] = config.ArtifactSource{Type: "github_release", Match: "image-*.iso"}
			cfg.Manifest.Artifacts[1].Sources["rva23"] = config.ArtifactSource{Type: "github_release", Match: "optional-*.iso"}
			now := time.Now()
			p, err := prepare(cfg, Options{Targets: []string{"rva23"}, Date: "2026-11-23"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p.Root, 0755); err != nil {
				t.Fatal(err)
			}
			err = createTarget(context.Background(), &output, store, cfg, p, store.directory, p.Targets[0], now)
			if imageCount != 1 {
				if err == nil {
					t.Fatal("missing or ambiguous required asset was published")
				}
				if _, err := os.Stat(filepath.Join(p.Target, "rva23")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("incomplete target became public")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page := readText(t, filepath.Join(p.Target, "rva23", "index.html"))
			if !strings.Contains(page, `<td class="missing">optional.img</td><td></td><td></td>`) || strings.Contains(page, `href="optional.img"`) {
				t.Fatal("missing optional release asset was not shown correctly")
			}
		})
	}
}

func TestReleaseDryRunShowsDiscoveryWithoutWrites(t *testing.T) {
	cfg := dailyConfig(t, "https://example.org")
	cfg.Manifest.Artifacts[0].Sources["rva23"] = config.ArtifactSource{Type: "github_release", Match: "image-rva23-*.iso"}
	var output bytes.Buffer
	if err := Create(context.Background(), cfg, Options{Targets: []string{"rva23"}, Date: "2026-11-23", DryRun: true}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "https://api.github.com/repos/example-owner/example-images/releases/latest") || !strings.Contains(output.String(), "image-rva23-*.iso") {
		t.Fatal("dry-run omitted the release endpoint or asset pattern")
	}
	if !strings.Contains(output.String(), "token: not configured") {
		t.Fatal("dry-run omitted the empty token status")
	}
	if _, err := os.Stat(cfg.Paths.ReleasesRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry-run wrote release content")
	}
}

package dailybuild

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// githubAPI keeps authentication in request headers and uses a replaceable base for tests.
type githubAPI struct {
	client   *http.Client
	base     string
	settings config.GitHubSource
	token    string
}

// workflowRun contains the completion data provided by GitHub's workflow runs API.
type workflowRun struct {
	ID         int64     `json:"id"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// githubArtifact identifies a nonexpired archive belonging to the selected run.
type githubArtifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
}

// request adds the token only to requests sent to the configured API origin.
func (g *githubAPI) request(ctx context.Context, endpoint string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+endpoint, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+g.token)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	r.Header.Set("User-Agent", "ruyi-deck/1")
	return r, nil
}

// jsonPage bounds API response sizes and preserves authentication errors without response bodies.
func (g *githubAPI) jsonPage(ctx context.Context, endpoint string, value any) (bool, error) {
	r, err := g.request(ctx, endpoint)
	if err != nil {
		return false, err
	}
	response, err := g.client.Do(r)
	if err != nil {
		return false, fmt.Errorf("GitHub API request: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GitHub API: HTTP %d (check repository Actions read access, token, and rate limits)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return false, err
	}
	if len(data) > 8<<20 {
		return false, fmt.Errorf("GitHub API response is too large")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return false, fmt.Errorf("decode GitHub API response: %w", err)
	}
	return strings.Contains(response.Header.Get("Link"), `rel="next"`), nil
}

// repositoryPath escapes independently validated owner and repository components.
func (g *githubAPI) repositoryPath() string {
	return "/repos/" + url.PathEscape(g.settings.Owner) + "/" + url.PathEscape(g.settings.Repository)
}

// latestRun scans successful-run pages and compares completion update timestamps.
func (g *githubAPI) latestRun(ctx context.Context) (workflowRun, error) {
	var latest workflowRun
	for page := 1; page <= 100; page++ {
		endpoint := g.repositoryPath() + "/actions/workflows/" + url.PathEscape(g.settings.Workflow) + "/runs?status=success&per_page=100&page=" + strconv.Itoa(page)
		var result struct {
			Runs []workflowRun `json:"workflow_runs"`
		}
		next, err := g.jsonPage(ctx, endpoint, &result)
		if err != nil {
			return latest, err
		}
		for _, run := range result.Runs {
			if run.Status != "completed" || run.Conclusion != "success" || run.UpdatedAt.IsZero() {
				continue
			}
			if run.UpdatedAt.After(latest.UpdatedAt) || (run.UpdatedAt.Equal(latest.UpdatedAt) && run.ID > latest.ID) {
				latest = run
			}
		}
		if !next {
			if latest.ID == 0 {
				return latest, fmt.Errorf("workflow %s has no completed successful run", g.settings.Workflow)
			}
			return latest, nil
		}
	}
	return latest, fmt.Errorf("GitHub workflow history exceeds the pagination limit")
}

// artifact requires exactly one nonexpired artifact with the configured name.
func (g *githubAPI) artifact(ctx context.Context, runID int64) (githubArtifact, error) {
	var matches []githubArtifact
	for page := 1; page <= 100; page++ {
		endpoint := fmt.Sprintf("%s/actions/runs/%d/artifacts?per_page=100&page=%d", g.repositoryPath(), runID, page)
		var result struct {
			Artifacts []githubArtifact `json:"artifacts"`
		}
		next, err := g.jsonPage(ctx, endpoint, &result)
		if err != nil {
			return githubArtifact{}, err
		}
		for _, a := range result.Artifacts {
			if a.Name == g.settings.ArtifactName && !a.Expired && a.ID > 0 {
				matches = append(matches, a)
			}
		}
		if !next {
			if len(matches) != 1 {
				return githubArtifact{}, fmt.Errorf("run %d: expected one nonexpired artifact %q, found %d", runID, g.settings.ArtifactName, len(matches))
			}
			return matches[0], nil
		}
	}
	return githubArtifact{}, fmt.Errorf("GitHub artifact listing exceeds the pagination limit")
}

// downloadZIP selects a run afresh for this invocation, then follows the authenticated download.
func (g *githubAPI) downloadZIP(ctx context.Context, target string) error {
	if g.token == "" {
		return fmt.Errorf("GitHub token environment variable %s is not set", g.settings.TokenEnv)
	}
	run, err := g.latestRun(ctx)
	if err != nil {
		return err
	}
	a, err := g.artifact(ctx, run.ID)
	if err != nil {
		return err
	}
	r, err := g.request(ctx, fmt.Sprintf("%s/actions/artifacts/%d/zip", g.repositoryPath(), a.ID))
	if err != nil {
		return err
	}
	r.Header.Set("Accept-Encoding", "identity")
	return downloadRequest(ctx, g.client, r, target)
}

const maxExtractedSize int64 = 16 << 30

// safeArchivePath prevents traversal and platform-dependent backslash paths.
func safeArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// extractZIP requires one regular matching member and never extracts an archive tree.
func extractZIP(ctx context.Context, archive, pattern, target string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open artifact ZIP: %w", err)
	}
	defer z.Close()
	var matches []*zip.File
	for _, entry := range z.File {
		if !safeArchivePath(entry.Name) || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe ZIP member path or symlink")
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("ZIP contains a nonregular member")
		}
		name := entry.Name
		if !strings.Contains(pattern, "/") {
			name = path.Base(name)
		}
		matched, err := path.Match(pattern, name)
		if err != nil {
			return err
		}
		if matched {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("ZIP pattern %q matched %d files; expected exactly one", pattern, len(matches))
	}
	member := matches[0]
	if member.UncompressedSize64 == 0 || member.UncompressedSize64 > uint64(maxExtractedSize) {
		return fmt.Errorf("ZIP member is empty or exceeds the extraction limit")
	}
	in, err := member.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	size, err := fsutil.Copy(ctx, out, io.LimitReader(in, maxExtractedSize+1))
	if err != nil {
		return fmt.Errorf("extract ZIP member: %w", err)
	}
	if size != int64(member.UncompressedSize64) {
		return fmt.Errorf("ZIP member length mismatch")
	}
	if err := out.Close(); err != nil {
		return err
	}
	if !member.Modified.IsZero() {
		return os.Chtimes(target, member.Modified, member.Modified)
	}
	return nil
}

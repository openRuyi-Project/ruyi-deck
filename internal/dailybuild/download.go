package dailybuild

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// statusError preserves the HTTP status so optional missing files can be recognized.
type statusError struct {
	Code int
}

// Error describes a failed HTTP response without including response body secrets.
func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.Code, http.StatusText(e.Code))
}

// newHTTPClient allows large streaming downloads while bounding network waits.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   6 * time.Hour,
		CheckRedirect: func(req *http.Request, previous []*http.Request) error {
			if req.URL.Host != previous[0].URL.Host {
				req.Header.Del("Authorization")
			}
			if len(previous) >= 10 {
				return fmt.Errorf("too many download redirects")
			}
			if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.User != nil {
				return fmt.Errorf("download redirect must use HTTP(S) without credentials")
			}
			if previous[len(previous)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing download redirect from HTTPS to HTTP")
			}
			return nil
		},
	}
}

// download streams to a temporary name and renames only a complete, nonempty file.
func download(ctx context.Context, client *http.Client, address, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	request.Header.Set("User-Agent", "ruyi-deck/1")
	request.Header.Set("Accept-Encoding", "identity")
	return downloadRequest(ctx, client, request, target)
}

// requestError removes signed redirect URLs from network error messages.
func requestError(err error) error {
	var e *url.Error
	if errors.As(err, &e) {
		return e.Err
	}
	return err
}

// downloadRequest also accepts authenticated GitHub API requests.
func downloadRequest(ctx context.Context, client *http.Client, request *http.Request, target string) error {
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download request: %w", requestError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &statusError{Code: response.StatusCode}
	}
	// Manifest names cannot start with a dot, so this private staging name cannot
	// collide with any final artifact. OpenFile applies the running user's umask.
	partialName := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".part")
	partial, err := os.OpenFile(partialName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create partial download: %w", err)
	}
	defer func() {
		partial.Close()
		os.Remove(partialName) // The complete staging tree is also removed on failure.
	}()
	size, err := fsutil.Copy(ctx, partial, response.Body)
	if err != nil {
		return fmt.Errorf("read download: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("downloaded file is empty")
	}
	if response.ContentLength >= 0 && size != response.ContentLength {
		return fmt.Errorf("incomplete download: got %d bytes, expected %d", size, response.ContentLength)
	}
	if err := partial.Close(); err != nil {
		return fmt.Errorf("close downloaded file: %w", err)
	}
	if err := renameDownload(ctx, partialName, target); err != nil {
		return err
	}
	if modified, err := http.ParseTime(response.Header.Get("Last-Modified")); err == nil {
		if err := os.Chtimes(target, modified, modified); err != nil {
			return fmt.Errorf("set downloaded file timestamp: %w", err)
		}
	}
	return nil
}

// renameDownload installs a complete download under the manifest's final name.
func renameDownload(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("refusing to overwrite staged file: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect final download filename: %w", err)
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("rename download to %s: %w", target, err)
	}
	return nil
}

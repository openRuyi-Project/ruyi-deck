package dailybuild

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// rpmCandidate records an exact package match and its RPM-comparable version.
type rpmCandidate struct {
	URL string
	EVR rpmVersion
}

// repositoryPackage decodes only package fields needed for OVMF discovery.
type repositoryPackage struct {
	Name         string `xml:"name"`
	Architecture string `xml:"arch"`
	Version      struct {
		Epoch   string `xml:"epoch,attr"`
		Version string `xml:"ver,attr"`
		Release string `xml:"rel,attr"`
	} `xml:"version"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
}

// getResponse checks status before a metadata or directory body is read.
func getResponse(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("User-Agent", "ruyi-deck/1")
	response, err := client.Do(r)
	if err != nil {
		return nil, requestError(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, &statusError{Code: response.StatusCode}
	}
	return response, nil
}

// discoverRPM falls back to a directory index when metadata cannot be used.
func discoverRPM(ctx context.Context, client *http.Client, base, packageName, architecture string) (rpmCandidate, error) {
	candidate, metadataErr := discoverMetadata(ctx, client, base, packageName, architecture)
	if metadataErr == nil {
		return candidate, nil
	}
	if ctx.Err() != nil {
		return rpmCandidate{}, ctx.Err()
	}
	candidate, indexErr := discoverIndex(ctx, client, base, packageName, architecture)
	if indexErr != nil {
		return rpmCandidate{}, errors.Join(metadataErr, fmt.Errorf("directory index: %w", indexErr))
	}
	return candidate, nil
}

// discoverMetadata tries the repository primary XML before the directory fallback.
func discoverMetadata(ctx context.Context, client *http.Client, base, packageName, architecture string) (rpmCandidate, error) {
	metadataURL, err := config.JoinURL(base, "repodata/repomd.xml")
	if err != nil {
		return rpmCandidate{}, err
	}
	response, err := getResponse(ctx, client, metadataURL)
	if err != nil {
		var status *statusError
		if errors.As(err, &status) && (status.Code == 404 || status.Code == 410) {
			return rpmCandidate{}, fmt.Errorf("repository metadata is unavailable")
		}
		return rpmCandidate{}, fmt.Errorf("read repository metadata: %w", err)
	}
	defer response.Body.Close()
	var metadata struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&metadata); err != nil {
		return rpmCandidate{}, err
	}
	var primary string
	for _, data := range metadata.Data {
		if data.Type == "primary" {
			if primary != "" {
				return rpmCandidate{}, fmt.Errorf("multiple primary metadata files")
			}
			primary = data.Location.Href
		}
	}
	if primary == "" {
		return rpmCandidate{}, fmt.Errorf("repository has no primary metadata")
	}
	address, err := config.JoinURL(base, primary)
	if err != nil {
		return rpmCandidate{}, err
	}
	response2, err := getResponse(ctx, client, address)
	if err != nil {
		return rpmCandidate{}, err
	}
	defer response2.Body.Close()
	var reader io.Reader = response2.Body
	switch {
	case strings.HasSuffix(primary, ".gz"):
		z, err := gzip.NewReader(response2.Body)
		if err != nil {
			return rpmCandidate{}, err
		}
		defer z.Close()
		reader = z
	case strings.HasSuffix(primary, ".bz2"):
		reader = bzip2.NewReader(response2.Body)
	case strings.HasSuffix(primary, ".xml"):
	default:
		return rpmCandidate{}, fmt.Errorf("unsupported primary metadata compression")
	}
	return selectPrimary(io.LimitReader(reader, 256<<20), base, packageName, architecture)
}

// selectPrimary streams package records rather than retaining an entire repository in memory.
func selectPrimary(reader io.Reader, base, packageName, architecture string) (rpmCandidate, error) {
	decoder := xml.NewDecoder(reader)
	var latest rpmCandidate
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return latest, fmt.Errorf("decode primary metadata: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "package" {
			continue
		}
		var p repositoryPackage
		if err := decoder.DecodeElement(&p, &start); err != nil {
			return latest, err
		}
		if p.Name != packageName || p.Architecture != architecture {
			continue
		}
		epoch := uint64(0)
		if p.Version.Epoch != "" {
			epoch, err = strconv.ParseUint(p.Version.Epoch, 10, 64)
			if err != nil {
				return latest, err
			}
		}
		if p.Version.Version == "" || p.Version.Release == "" {
			return latest, fmt.Errorf("OVMF package has no version or release")
		}
		address, err := config.JoinURL(base, p.Location.Href)
		if err != nil {
			return latest, err
		}
		candidate := rpmCandidate{URL: address, EVR: rpmVersion{epoch, p.Version.Version, p.Version.Release}}
		if latest.URL == "" || compareRPM(candidate.EVR, latest.EVR) > 0 {
			latest = candidate
		}
	}
	if latest.URL == "" {
		return latest, fmt.Errorf("package %s for %s not found in metadata", packageName, architecture)
	}
	return latest, nil
}

var indexLinks = regexp.MustCompile(`(?i)<a\b[^>]*\bhref\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var rpmPart = regexp.MustCompile(`^[A-Za-z0-9._+~^]+$`)

// indexCandidate excludes debuginfo, source packages, other architectures and subdirectories.
func indexCandidate(base, href, packageName, architecture string) (rpmCandidate, bool) {
	u, err := url.Parse(html.UnescapeString(href))
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return rpmCandidate{}, false
	}
	name := u.Path
	if name != path.Base(name) || strings.Contains(name, "\\") {
		return rpmCandidate{}, false
	}
	prefix, suffix := packageName+"-", "."+architecture+".rpm"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return rpmCandidate{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	separator := strings.LastIndex(body, "-")
	if separator < 1 {
		return rpmCandidate{}, false
	}
	version, release := body[:separator], body[separator+1:]
	if !rpmPart.MatchString(version) || !rpmPart.MatchString(release) {
		return rpmCandidate{}, false
	}
	address, err := config.JoinURL(base, name)
	if err != nil {
		return rpmCandidate{}, false
	}
	return rpmCandidate{URL: address, EVR: rpmVersion{Version: version, Release: release}}, true
}

// discoverIndex recognizes quoted links and compares parsed RPM version components.
func discoverIndex(ctx context.Context, client *http.Client, base, packageName, architecture string) (rpmCandidate, error) {
	response, err := getResponse(ctx, client, base)
	if err != nil {
		return rpmCandidate{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return rpmCandidate{}, err
	}
	if len(data) > 16<<20 {
		return rpmCandidate{}, fmt.Errorf("repository directory index exceeds the size limit")
	}
	var latest rpmCandidate
	for _, match := range indexLinks.FindAllStringSubmatch(string(data), -1) {
		href := match[1]
		if href == "" {
			href = match[2]
		}
		candidate, ok := indexCandidate(base, href, packageName, architecture)
		if ok && (latest.URL == "" || compareRPM(candidate.EVR, latest.EVR) > 0) {
			latest = candidate
		}
	}
	if latest.URL == "" {
		return latest, fmt.Errorf("no exact %s package for %s in repository index", packageName, architecture)
	}
	return latest, nil
}

// boundedBuffer limits external-program diagnostics.
type boundedBuffer struct {
	bytes.Buffer
	limit int
}

// Write refuses oversized output rather than allowing an unbounded allocation.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("external program output exceeds limit")
	}
	return b.Buffer.Write(p)
}

// externalRun allows extraction tests to replace external processes with in-memory fakes.
type externalRun func(context.Context, io.Reader, io.Writer, string, ...string) error

// runExternal streams data through a program without constructing a shell command.
func runExternal(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, stderr.String())
	}
	return nil
}

// convertRPM runs rpm2cpio once and keeps its output private until extraction succeeds.
func convertRPM(ctx context.Context, rpmFile, cpioFile string, run externalRun) error {
	out, err := os.OpenFile(cpioFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := run(ctx, nil, out, "rpm2cpio", rpmFile); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return checkFile(cpioFile)
}

// cpioMember validates newc headers, type and size instead of trusting a text listing.
func cpioMember(ctx context.Context, archive, internal string) (string, int64, error) {
	in, err := os.Open(archive)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	reader := bufio.NewReader(in)
	var found string
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		header := make([]byte, 110)
		if _, err := io.ReadFull(reader, header); err != nil {
			return "", 0, fmt.Errorf("read cpio header: %w", err)
		}
		if string(header[:6]) != "070701" && string(header[:6]) != "070702" {
			return "", 0, fmt.Errorf("unsupported cpio format; expected RPM newc payload")
		}
		number := func(offset int) (uint32, error) {
			var raw [4]byte
			if _, err := hex.Decode(raw[:], header[offset:offset+8]); err != nil {
				return 0, err
			}
			return binary.BigEndian.Uint32(raw[:]), nil
		}
		mode, err := number(14)
		if err != nil {
			return "", 0, err
		}
		length, err := number(54)
		if err != nil {
			return "", 0, err
		}
		nameLength, err := number(94)
		if err != nil || nameLength < 1 || nameLength > 4096 {
			return "", 0, fmt.Errorf("invalid cpio member name length")
		}
		nameBytes := make([]byte, nameLength)
		if _, err := io.ReadFull(reader, nameBytes); err != nil {
			return "", 0, err
		}
		if nameBytes[len(nameBytes)-1] != 0 {
			return "", 0, fmt.Errorf("cpio member name lacks terminator")
		}
		name := string(nameBytes[:len(nameBytes)-1])
		if _, err := io.CopyN(io.Discard, reader, int64((4-(110+nameLength)%4)%4)); err != nil {
			return "", 0, err
		}
		if name == "TRAILER!!!" {
			break
		}
		if !safeArchivePath(name) {
			return "", 0, fmt.Errorf("unsafe cpio member path")
		}
		if strings.TrimPrefix(name, "./") == internal {
			if found != "" {
				return "", 0, fmt.Errorf("RPM internal path %s matched multiple members", internal)
			}
			if mode&0170000 != 0100000 || length == 0 {
				return "", 0, fmt.Errorf("RPM internal path must be a nonempty regular file: %s", internal)
			}
			found, size = name, int64(length)
		}
		copied, err := fsutil.Copy(ctx, io.Discard, io.LimitReader(reader, int64(length)))
		if err != nil {
			return "", 0, err
		}
		if copied != int64(length) {
			return "", 0, fmt.Errorf("truncated cpio member: %s", name)
		}
		if _, err := io.CopyN(io.Discard, reader, int64((4-length%4)%4)); err != nil {
			return "", 0, err
		}
	}
	if found == "" {
		return "", 0, fmt.Errorf("RPM internal path not found: %s", internal)
	}
	return found, size, nil
}

// extractCPIO writes only the validated member to stdout, never unpacking archive paths.
func extractCPIO(ctx context.Context, archive, internal, target string, run externalRun) error {
	member, expected, err := cpioMember(ctx, archive, internal)
	if err != nil {
		return err
	}
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := run(ctx, in, out, "cpio", "-i", "--quiet", "--to-stdout", "--no-absolute-filenames", "--", member); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := checkFile(target); err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if info.Size() != expected {
		return fmt.Errorf("cpio extracted length differs from validated member size")
	}
	return nil
}

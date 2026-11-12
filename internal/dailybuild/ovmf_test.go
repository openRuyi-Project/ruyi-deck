package dailybuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRPMVersionComparison(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.9", "1.10", -1}, {"1.001", "1.1", 0}, {"1.0~rc1", "1.0", -1},
		{"1.0", "1.0^git1", -1}, {"1.0^git1", "1.0.1", -1}, {"1.0^git2", "1.0^git1", 1},
		{"1a", "1.0", -1}, {"1.0+", "1.0", 0}, {"202602", "202601", 1},
	}
	for _, tc := range cases {
		got := rpmvercmp(tc.left, tc.right)
		if (got < 0 && tc.want >= 0) || (got > 0 && tc.want <= 0) || (got == 0 && tc.want != 0) {
			t.Errorf("rpmvercmp(%q,%q) = %d; want sign %d", tc.left, tc.right, got, tc.want)
		}
		if got != -rpmvercmp(tc.right, tc.left) {
			t.Errorf("comparison is asymmetric: %+v", tc)
		}
	}
	if compareRPM(rpmVersion{Epoch: 1, Version: "1", Release: "1"}, rpmVersion{Version: "9999", Release: "9"}) <= 0 {
		t.Fatal("epoch ignored")
	}
	if compareRPM(rpmVersion{Version: "202602", Release: "13.14.or20"}, rpmVersion{Version: "202602", Release: "13.9.or20"}) <= 0 {
		t.Fatal("release compared lexically")
	}
}

func TestRPMDiscoveryMetadataAndIndex(t *testing.T) {
	for _, useMetadata := range []bool{true, false} {
		t.Run(fmt.Sprintf("metadata-%t", useMetadata), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repodata/repomd.xml":
					if !useMetadata {
						http.NotFound(w, r)
						return
					}
					io.WriteString(w, `<repomd><data type="primary"><location href="repodata/primary.xml"/></data></repomd>`)
				case "/repodata/primary.xml":
					io.WriteString(w, `<metadata><package><name>ovmf</name><arch>riscv64</arch><version epoch="0" ver="202602" rel="13.9.or20"/><location href="old.rpm"/></package><package><name>ovmf-debuginfo</name><arch>riscv64</arch><version epoch="0" ver="999999" rel="1"/><location href="debug.rpm"/></package><package><name>ovmf</name><arch>riscv64</arch><version epoch="0" ver="202602" rel="13.14.or20"/><location href="new.rpm"/></package></metadata>`)
				case "/":
					io.WriteString(w, `<a href="ovmf-202602-13.9.or20.riscv64.rpm">old</a><a href='ovmf-202602-13.14.or20.riscv64.rpm'>new</a><a href="ovmf-debuginfo-999999-1.riscv64.rpm">debug</a><a href="ovmf-999999-1.x86_64.rpm">wrong arch</a><a href="../ovmf-999999-1.riscv64.rpm">unsafe</a>`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			candidate, err := discoverRPM(context.Background(), server.Client(), server.URL+"/", "ovmf", "riscv64")
			if err != nil {
				t.Fatal(err)
			}
			if candidate.EVR.Release != "13.14.or20" {
				t.Fatalf("latest = %+v", candidate)
			}
		})
	}
}

// newcMember creates a minimal RPM-style cpio entry for extraction validation.
func newcMember(out *bytes.Buffer, name string, mode uint32, content string) {
	fmt.Fprintf(out, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x", 1, mode, 0, 0, 1, 0, len(content), 0, 0, 0, 0, len(name)+1, 0)
	out.WriteString(name)
	out.WriteByte(0)
	for out.Len()%4 != 0 {
		out.WriteByte(0)
	}
	out.WriteString(content)
	for out.Len()%4 != 0 {
		out.WriteByte(0)
	}
}

func TestRPMExtractionWithFakeExternalPrograms(t *testing.T) {
	var payload bytes.Buffer
	newcMember(&payload, "./usr/share/ovmf/virt_code.fd", 0100644, "firmware")
	newcMember(&payload, "TRAILER!!!", 0, "")
	directory := t.TempDir()
	archive := filepath.Join(directory, "package.cpio")
	target := filepath.Join(directory, "RISCV_VIRT_CODE.fd")
	var programs []string
	run := func(_ context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		programs = append(programs, name)
		switch name {
		case "rpm2cpio":
			_, err := out.Write(payload.Bytes())
			return err
		case "cpio":
			if args[len(args)-1] != "./usr/share/ovmf/virt_code.fd" || in == nil {
				return errors.New("incorrect extraction arguments")
			}
			_, err := io.WriteString(out, "firmware")
			return err
		default:
			return fmt.Errorf("unexpected external program %s", name)
		}
	}
	if err := convertRPM(context.Background(), "fake.rpm", archive, run); err != nil {
		t.Fatal(err)
	}
	if err := extractCPIO(context.Background(), archive, "usr/share/ovmf/virt_code.fd", target, run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(programs, ",") != "rpm2cpio,cpio" {
		t.Fatalf("programs = %v", programs)
	}
	if _, _, err := cpioMember(context.Background(), archive, "missing.fd"); err == nil {
		t.Fatal("missing firmware accepted")
	}
	fail := func(context.Context, io.Reader, io.Writer, string, ...string) error {
		return errors.New("program failed")
	}
	if err := convertRPM(context.Background(), "fake.rpm", filepath.Join(directory, "failed.cpio"), fail); err == nil {
		t.Fatal("rpm2cpio error ignored")
	}
	if err := extractCPIO(context.Background(), archive, "usr/share/ovmf/virt_code.fd", filepath.Join(directory, "failed.fd"), fail); err == nil {
		t.Fatal("cpio error ignored")
	}
}

func TestCPIORejectsSymlinksTraversalAndDuplicates(t *testing.T) {
	for _, mode := range []string{"symlink", "traversal", "duplicate"} {
		var payload bytes.Buffer
		switch mode {
		case "symlink":
			newcMember(&payload, "usr/share/ovmf/virt_code.fd", 0120777, "/etc/passwd")
		case "traversal":
			newcMember(&payload, "../usr/share/ovmf/virt_code.fd", 0100644, "x")
		case "duplicate":
			newcMember(&payload, "usr/share/ovmf/virt_code.fd", 0100644, "x")
			newcMember(&payload, "./usr/share/ovmf/virt_code.fd", 0100644, "y")
		}
		newcMember(&payload, "TRAILER!!!", 0, "")
		filename := filepath.Join(t.TempDir(), "unsafe.cpio")
		if err := os.WriteFile(filename, payload.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cpioMember(context.Background(), filename, "usr/share/ovmf/virt_code.fd"); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
}

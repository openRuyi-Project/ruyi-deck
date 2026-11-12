package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
)

func TestHelpAndVersionNeedNoConfiguration(t *testing.T) {
	for _, arguments := range [][]string{{"--help"}, {"repo", "sync", "rolling", "--help"}, {"releases", "create", "dailybuild", "--help"}, {"version"}, {"--version"}} {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--config", "/nonexistent/ruyi-deck.yaml"}, arguments...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if out.Len() == 0 {
			t.Fatalf("%v produced no output", arguments)
		}
	}
}

func TestDefaultConfigurationFollowsExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := defaultConfigPath(), filepath.Join(filepath.Dir(executable), "config", "config.yaml"); got != want {
		t.Fatalf("default = %s; want %s", got, want)
	}
}

func TestFlagErrorsHaveInputExitCode(t *testing.T) {
	for _, arguments := range [][]string{{"releases", "create", "dailybuild", "--arch", "rva23"}, {"repo", "sync", "rolling", "--target="}, {"releases", "create", "dailybuild", "--date="}} {
		root := newRootCommand()
		root.SetArgs(arguments)
		err := root.Execute()
		if err == nil || apperror.ExitCode(err) != 2 {
			t.Fatalf("%v: expected input error; got %v", arguments, err)
		}
	}
	if !strings.Contains(versionText(), Version) {
		t.Fatal("version metadata was not included")
	}
}

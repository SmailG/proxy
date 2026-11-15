//go:build darwin

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// recordLaunchctl replaces the launchctl seam and returns the calls made. It is
// not optional: the real one targets gui/<uid>/com.routatic.proxy, which a
// temporary HOME does not redirect, so an unguarded test here would SIGTERM the
// running proxy on the developer's machine and leave it unloaded.
func recordLaunchctl(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	orig := runLaunchctl
	runLaunchctl = func(args ...string) error {
		*calls = append(*calls, args)
		return nil
	}
	t.Cleanup(func() { runLaunchctl = orig })
	return calls
}

func TestEnableDisableAutostart_Darwin(t *testing.T) {
	// Setup temporary home directory
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	// Set a ROUTATIC_PROXY_* var with characters that need XML escaping.
	t.Setenv("ROUTATIC_PROXY_API_KEY", "sk-test&key<value>")
	calls := recordLaunchctl(t)

	configPath := "/tmp/mock-config.json"
	port := 9999

	// Enable autostart
	err := EnableAutostart(configPath, port)
	if err != nil {
		t.Fatalf("EnableAutostart failed: %v", err)
	}

	// Verify plist file was created
	plistPath := filepath.Join(tempHome, "Library", "LaunchAgents", LaunchAgent+".plist")
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		t.Fatalf("Expected plist file to exist at %s, but it does not", plistPath)
	}

	// Read and verify plist contents
	contentBytes, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("Failed to read plist file: %v", err)
	}
	content := string(contentBytes)

	// Check for key elements in the plist
	if !strings.Contains(content, "<key>Label</key>\n    <string>com.routatic.proxy</string>") {
		t.Errorf("Plist missing correct Label string")
	}
	if !strings.Contains(content, "<string>start</string>") {
		t.Errorf("Plist missing start command")
	}
	if !strings.Contains(content, "<string>--background</string>") {
		t.Errorf("Plist missing --background flag")
	}
	if !strings.Contains(content, "<string>--config</string>\n        <string>/tmp/mock-config.json</string>") {
		t.Errorf("Plist missing config path arguments")
	}
	if !strings.Contains(content, "<string>--port</string>\n        <string>9999</string>") {
		t.Errorf("Plist missing port arguments")
	}

	// Verify ROUTATIC_PROXY_* env var is present and XML-escaped.
	if !strings.Contains(content, "<key>ROUTATIC_PROXY_API_KEY</key>") {
		t.Errorf("Plist missing ROUTATIC_PROXY_API_KEY env var")
	}
	// The value should be XML-escaped: & -> &amp;, < -> &lt;, > -> &gt;
	if !strings.Contains(content, "<string>sk-test&amp;key&lt;value&gt;</string>") {
		t.Errorf("Plist missing XML-escaped API key value. Content:\n%s", content)
	}

	// On darwin, when the binary is a Homebrew stable path, the plist must
	// not contain a versioned Cellar path (launchd would break on brew upgrade).
	if isHomebrewStablePath(resolveExecutablePath(mustExecutable(t))) {
		if strings.Contains(content, "Cellar/") {
			t.Errorf("Plist must not contain Cellar/ for Homebrew installs, got:\n%s", content)
		}
	}

	// Disable autostart
	err = DisableAutostart()
	if err != nil {
		t.Fatalf("DisableAutostart failed: %v", err)
	}

	// Verify plist file was removed
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("Expected plist file to be deleted, but it still exists")
	}

	// Enable loads (bootout, then bootstrap) and Disable boots out. Both name the
	// real label, which is what the seam above has to stand in for.
	domain := "gui/" + strconv.Itoa(os.Getuid())
	want := [][]string{
		{"bootout", domain + "/" + LaunchAgent},
		{"bootstrap", domain, plistPath},
		{"bootout", domain + "/" + LaunchAgent},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("launchctl calls = %v, want %v", *calls, want)
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return p
}

// The wrapper names the failed subcommand because callers report the error
// without saying which operation they were performing, and "exit status 1" does
// not distinguish a failed bootstrap from a failed bootout.
//
// This is the one test in the package that calls the real launchctl. The
// subcommand is invented and names no domain and no service, so unlike every
// other invocation here it cannot reach a LaunchAgent; Run discards its usage
// output. It is the only way to exercise the wrapper, which recordLaunchctl
// replaces wholesale.
func TestRunLaunchctl_WrapsErrorsWithTheSubcommand(t *testing.T) {
	err := runLaunchctl("routatic-inert-subcommand-for-test")
	if err == nil {
		t.Fatal("an unrecognized subcommand was accepted")
	}
	if !strings.Contains(err.Error(), "launchctl routatic-inert-subcommand-for-test") {
		t.Errorf("err = %v, want it to name the subcommand", err)
	}
	// %w rather than %v: a caller that needs the exit status must still be able
	// to reach it through the wrap.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want the *exec.ExitError to survive wrapping", err)
	}
}

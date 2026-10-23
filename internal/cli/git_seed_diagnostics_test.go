package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGitSeedFailurePhaseAndPreservation(t *testing.T) {
	f := newGitCoherenceFixture(t)
	for _, phase := range []string{"clone", "checkout", "verify"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "work")
			if err := os.Mkdir(workdir, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "preserve.txt")
			mustWriteTestFile(t, marker, "existing workspace\n")
			plan := f.plan(t, f.b)
			switch phase {
			case "clone":
				plan.RemoteURL = filepath.Join(root, "missing-private-origin")
			case "checkout":
				plan.Target = strings.Repeat("f", 40)
			case "verify":
				plan.Tree = strings.Repeat("f", 40)
			}
			var out []byte
			var err error
			if runtime.GOOS == "windows" {
				out, err = runDecodedWindowsPowerShell(t, windowsGitSeed(workdir, plan))
			} else {
				out, err = exec.Command("bash", "-lc", remoteGitSeed(workdir, plan)).CombinedOutput()
			}
			if err == nil || !strings.Contains(string(out), "crabbox-git-seed phase="+phase) {
				t.Fatalf("missing %s failure diagnostic: err=%v output=%q", phase, err, out)
			}
			if phase == "checkout" && (exitCode(err) != gitSeedCheckoutFailureExitCode || exitCode(err) == 128) {
				t.Fatalf("checkout failure exit=%d want=%d and never git's raw fatal code: %v", exitCode(err), gitSeedCheckoutFailureExitCode, err)
			}
			var warning bytes.Buffer
			warnRemoteGitSeedFailure(&warning, string(out), err)
			first := strings.SplitN(warning.String(), "\n", 2)[0]
			if !strings.HasPrefix(first, "warning: remote git seed failed: phase="+phase+" reason=") {
				t.Fatalf("classified fields are not on the first line: %q", warning.String())
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "existing workspace\n" {
				t.Fatalf("failed seed changed sibling file: data=%q err=%v", data, err)
			}
			if entries, err := os.ReadDir(workdir); err != nil || len(entries) != 0 {
				t.Fatalf("failed seed changed empty destination: entries=%v err=%v", entries, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(root, ".seed*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("seed residue=%v err=%v", leftovers, err)
			}
		})
	}
}

func TestGitSeedDiagnosticLabelsAndPrivacy(t *testing.T) {
	fixtureURL := &url.URL{Scheme: "https", Host: "example.invalid", Path: "/private", User: url.UserPassword("fixture-user", "do-not-forward")}
	for _, tc := range []struct{ name, output, want string }{
		{"auth", "Authentication failed for " + fixtureURL.String(), "authentication/access"},
		{"prompt", "could not read Username for 'https://secret': terminal prompts disabled", "authentication/access"},
		{"dns", "Could not resolve host: private.example", "dns"},
		{"connection", "Failed to connect to private.example port 443", "connectivity"},
		{"tls", "SSL certificate problem: private certificate", "tls"},
		{"repo", "fatal: '/private/secret' does not appear to be a git repository", "repository/ref"},
		{"ref", "fatal: Remote branch secret not found in upstream origin", "repository/ref"},
		{"unknown", "private helper says secret\x1b[2J\runknown", "unknown"},
		{"localized", "fatal: dépôt secret introuvable", "unknown"},
		{"forged field", "crabbox-git-seed phase=private-secret\nreason=private-secret", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			warnRemoteGitSeedFailure(&out, "crabbox-git-seed phase=clone\n"+tc.output, errors.New("private transport error"))
			want := "warning: remote git seed failed: phase=clone reason=" + tc.want + " exit=unknown; continuing with file sync; Git metadata was not seeded or verified\n"
			if !strings.HasPrefix(out.String(), want) {
				t.Fatalf("warning=%q want prefix=%q", out.String(), want)
			}
			for _, line := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(out.String(), want), "\n"), "\n") {
				if line == "" {
					continue
				}
				if !strings.HasPrefix(line, "warning: remote git seed detail (untrusted): ") {
					t.Fatalf("unlabeled detail line: %q", line)
				}
				if strings.ContainsAny(line, "\x1b\r") {
					t.Fatalf("detail line carries terminal control characters: %q", line)
				}
			}
			if strings.Contains(out.String(), "do-not-forward") || strings.Contains(out.String(), "fixture-user") {
				t.Fatalf("warning leaked URL credentials: %q", out.String())
			}
		})
	}
	for _, tc := range []struct{ output, want string }{
		{"crabbox-git-seed phase=prerequisite\nprivate command lookup", "reason=missing-git"},
		{"crabbox-git-seed phase=clone\nSSL certificate problem\ncrabbox-git-seed phase=verify\n", "reason=verification"},
	} {
		var out bytes.Buffer
		warnRemoteGitSeedFailure(&out, tc.output, context.Canceled)
		if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "exit=cancelled") {
			t.Fatalf("warning=%q", out.String())
		}
	}
}

// A seed failure must name git's own cause. The regression this covers reported
// phase=checkout reason=unknown exit=128 while git's explanation was discarded.
func TestGitSeedDiagnosticSurfacesSanitizedDetail(t *testing.T) {
	var out bytes.Buffer
	capture := "crabbox-git-seed phase=checkout\n" +
		"fatal: unable to checkout working tree\n" +
		"error: invalid path 'drivers/gpu'\n" +
		"\x1b[31mwarning: https://user:tok3n@example.invalid/repo.git was rejected\x1b[0m\r\n"
	warnRemoteGitSeedFailure(&out, capture, errors.New("exit status 128"))
	text := out.String()
	if !strings.Contains(text, "phase=checkout reason=checkout exit=unknown") {
		t.Fatalf("warning did not classify the checkout failure: %q", text)
	}
	for _, want := range []string{
		"warning: remote git seed detail (untrusted): fatal: unable to checkout working tree\n",
		"warning: remote git seed detail (untrusted): error: invalid path 'drivers/gpu'\n",
		"https://***@example.invalid/repo.git",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "tok3n") || strings.ContainsAny(text, "\x1b\r") {
		t.Fatalf("detail was not sanitized: %q", text)
	}
}

func TestGitSeedDiagnosticDetailIsBounded(t *testing.T) {
	var out bytes.Buffer
	lines := []string{"crabbox-git-seed phase=clone"}
	for i := 0; i < 200; i++ {
		lines = append(lines, "remote line "+strconv.Itoa(i)+" "+strings.Repeat("x", 2000))
	}
	warnRemoteGitSeedFailure(&out, strings.Join(lines, "\n"), errors.New("exit status 128"))
	detail := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "warning: remote git seed detail (untrusted): ") {
			detail++
			if len(line) > gitSeedDetailLineLimit+64 {
				t.Fatalf("detail line not bounded: %d bytes", len(line))
			}
		}
	}
	if detail < 1 || detail > gitSeedDetailLineCount+1 || len(out.String()) > gitSeedDetailByteLimit+4096 {
		t.Fatalf("detail block not bounded: lines=%d bytes=%d", detail, len(out.String()))
	}
	if !strings.Contains(out.String(), "... (further detail omitted)") {
		t.Fatalf("omitted detail was not reported: %q", out.String())
	}
}

func TestGitSeedDiagnosticReasonsCoverGitDieClasses(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"missing object", "fatal: reference is not a tree: 0123456789", "repository/objects"},
		{"promisor fetch", "fatal: could not fetch 0123456789 from promisor remote", "repository/objects"},
		{"working tree", "fatal: unable to checkout working tree", "checkout"},
		{"disk full", "fatal: write error: No space left on device", "storage"},
		{"ownership", "fatal: detected dubious ownership in repository at '/work/repo'", "ownership"},
		{"http status", "error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429", "connectivity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			warnRemoteGitSeedFailure(&out, "crabbox-git-seed phase=checkout\n"+tc.output, errors.New("exit status 128"))
			if !strings.HasPrefix(out.String(), "warning: remote git seed failed: phase=checkout reason="+tc.want+" exit=unknown;") {
				t.Fatalf("warning=%q want reason=%s", out.String(), tc.want)
			}
		})
	}
}

// A checkout that fails on the transport path has published nothing, so the
// transport classifier decides the disposition instead of git's own 128
// aborting a sync that plain manifest mode can complete.
func TestGitSeedCheckoutFailureDisposition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture for process exit status")
	}
	for _, tc := range []struct {
		name, output string
		wantReason   string
		wantFallback bool
	}{
		{
			name:         "promisor transport failure",
			output:       "crabbox-git-seed phase=checkout\nfatal: unable to access 'https://example.test/repo': Failed to connect to example.test port 443 after 133131 ms: Could not connect to server\nfatal: could not fetch 244d034463908327706bedfb13daa49843052b95 from promisor remote",
			wantReason:   "origin_unavailable",
			wantFallback: true,
		},
		{
			name:         "content failure stays fatal",
			output:       "crabbox-git-seed phase=checkout\nfatal: unable to checkout working tree\nerror: invalid path 'drivers/gpu'",
			wantFallback: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := exec.Command("sh", "-c", "exit "+strconv.Itoa(gitSeedCheckoutFailureExitCode)).Run()
			if exitCode(err) != gitSeedCheckoutFailureExitCode {
				t.Fatalf("fixture exit=%d err=%v", exitCode(err), err)
			}
			plan := gitCoherencePlan{RemoteURL: "https://example.test/repo.git", Target: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}
			reason, fallback := gitSeedRuntimeFallbackResult(plan, tc.output, err)
			if fallback != tc.wantFallback || reason != tc.wantReason {
				t.Fatalf("fallback=%t reason=%q want fallback=%t reason=%q", fallback, reason, tc.wantFallback, tc.wantReason)
			}
		})
	}
}

func TestGitSeedCaptureLimitPreservesExecutionAndRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	for _, tc := range []struct {
		name, body string
		wantCalls  int
		wantError  bool
	}{
		{"remote failure", "exit 128", 1, true},
		{"transport failure", "exit 255", 2, true},
		{"success", "exit 0", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			drained := filepath.Join(dir, "drained")
			script := "#!/bin/sh\nprintf 'call\\n' >> " + shellQuote(calls) + "\nprintf 'crabbox-git-seed phase=clone\\n'\nhead -c 1048576 /dev/zero\nprintf secret >&2\nprintf complete > " + shellQuote(drained) + "\n" + tc.body + "\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := runIdempotentSSHCombinedOutputLimit(ctx, SSHTarget{Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}}, "true", 0, gitSeedDiagnosticLimit)
			if (err != nil) != tc.wantError || out != "" {
				t.Fatalf("out length=%d err=%v", len(out), err)
			}
			data, readErr := os.ReadFile(calls)
			if readErr != nil || strings.Count(string(data), "call\n") != tc.wantCalls {
				t.Fatalf("calls=%q err=%v", data, readErr)
			}
			if data, err := os.ReadFile(drained); err != nil || string(data) != "complete" {
				t.Fatalf("producer did not drain: %q %v", data, err)
			}
			if tc.wantError {
				var warning bytes.Buffer
				warnRemoteGitSeedFailure(&warning, out, err)
				if !strings.Contains(warning.String(), "phase=unknown reason=unknown") || strings.Contains(warning.String(), "secret") {
					t.Fatalf("warning=%q", warning.String())
				}
			}
		})
	}
	capture := newSynchronizedBuffer(gitSeedDiagnosticLimit)
	n, err := io.Copy(&capture, strings.NewReader(strings.Repeat("x", 1<<20)))
	retained, truncated := capture.boundedString()
	if err != nil || n != 1<<20 || len(retained) > gitSeedDiagnosticLimit || !truncated || capture.String() != "" {
		t.Fatalf("capture n=%d err=%v retained=%d truncated=%v", n, err, len(retained), truncated)
	}
}

func TestGitOriginAttemptRetainsBoundedTruncatedDiagnosticsWithoutFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'fatal: Authentication failed\\n'\nhead -c 1048576 /dev/zero\nexit 78\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := runIdempotentSSHGitOriginAttempt(t.Context(), SSHTarget{Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}}, "true", 0)
	if err == nil || len(out) != gitSeedDiagnosticLimit {
		t.Fatalf("diagnostic bytes=%d err=%v", len(out), err)
	}
	var truncated *gitOriginDiagnosticsTruncatedError
	if !errors.As(err, &truncated) {
		t.Fatalf("truncated diagnostic error type lost: %T", err)
	}
	if reason, fallback := gitOriginRuntimeFallbackResult("https://example.test/repo.git", out, err); fallback || reason != "" {
		t.Fatalf("truncated output authorized fallback=%t reason=%q", fallback, reason)
	}
	var warning bytes.Buffer
	reportRemoteGitSeedFailure(&warning, out, err, "aborting before file sync")
	want := "warning: remote git seed failed: phase=unknown reason=unknown exit=78; aborting before file sync; Git metadata was not seeded or verified\n"
	if warning.String() != want {
		t.Fatalf("warning=%q want=%q", warning.String(), want)
	}
}

func TestActionsSeedDiagnosticFailsSafely(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture for Actions hydration")
	}
	f := newGitCoherenceFixture(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	ssh := `#!/bin/sh
last=
for arg do last="$arg"; done
case "$last" in
  *'git clone --quiet'*)
    printf 'seed\n' >> "$SEED_TEST_CALLS"
    printf 'crabbox-git-seed phase=clone\n'
    printf 'fatal: repository secret-private-endpoint not found\n' >&2
    exit 128 ;;
  *'update-ref --no-deref HEAD'*) printf 'finalize\n' >> "$SEED_TEST_CALLS" ;;
esac
cat >/dev/null
`
	for name, script := range map[string]string{
		"ssh":   ssh,
		"rsync": "#!/bin/sh\nprintf 'rsync\\n' >> \"$SEED_TEST_CALLS\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SEED_TEST_CALLS", calls)
	cfg := baseConfig()
	cfg.Sync.BaseRef = "main"
	repo := Repo{Root: f.source, Name: "repo", RemoteURL: f.origin, Head: f.b, BaseRef: "main"}
	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	_, err := app.syncLocalActionsWorkspace(context.Background(), cfg, repo, SSHTarget{User: "builder", Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}, TargetOS: targetLinux}, "/work/repo", false)
	if err == nil || !strings.Contains(err.Error(), "remote git seed failed") {
		t.Fatalf("sync error=%v\n%s", err, stderr.String())
	}
	warning := "warning: remote git seed failed: phase=clone reason=repository/ref exit=128; aborting before file sync; Git metadata was not seeded or verified"
	if strings.Count(stderr.String(), warning) != 1 {
		t.Fatalf("unsafe or missing diagnostic: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: remote git seed detail (untrusted): fatal: repository secret-private-endpoint not found") {
		t.Fatalf("git's own explanation was discarded: %q", stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, "secret-private-endpoint") &&
			!strings.HasPrefix(line, "warning: remote git seed detail (untrusted): ") {
			t.Fatalf("remote text escaped the labeled detail block: %q", line)
		}
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "seed\n" {
		t.Fatalf("production transition order=%q err=%v", data, err)
	}
}

func TestGitOriginAttemptPublicationReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell SSH fixture")
	}
	for _, tc := range []struct {
		name, script string
		want         bool
	}{
		{"published", "printf '%s\\n' " + shellQuote(gitSeedPublishedReceipt), true},
		{"stderr-is-not-a-receipt", "printf '%s\\n' " + shellQuote(gitSeedPublishedReceipt) + " >&2", false},
		{"publication-not-completed", "printf 'crabbox-git-seed phase=publish\\n'", false},
		{"truncated", "printf '%s\\n' " + shellQuote(gitSeedPublishedReceipt) + "; head -c 1048576 /dev/zero", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			_, published, err := runIdempotentSSHGitOriginAttemptWithPublication(t.Context(), SSHTarget{Host: "fixture.invalid", Port: "22", FallbackPorts: []string{}}, "true", 0)
			if err != nil || published != tc.want {
				t.Fatalf("published=%t want=%t err=%v", published, tc.want, err)
			}
		})
	}
}

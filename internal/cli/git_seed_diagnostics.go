package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	gitSeedDiagnosticLimit = 16 << 10
	// The detail block is the remote Git/transport stderr that explains the
	// failure. Remote output is untrusted, including helper output and URLs, so
	// every line is stripped of terminal control characters, has URL credentials
	// redacted, carries a fixed label, and is bounded. The classified fields in
	// the warning line are still derived only from fixed markers, never from the
	// detail text.
	gitSeedDetailLineLimit = 512
	gitSeedDetailByteLimit = 2048
	gitSeedDetailLineCount = 8
)

func warnRemoteGitSeedFailure(w io.Writer, output string, err error) {
	reportRemoteGitSeedFailure(w, output, err, "continuing with file sync")
}

func reportRemoteGitSeedFailure(w io.Writer, output string, err error, disposition string) {
	// A truncated capture is discarded: its text cannot be classified or shown.
	if gitSeedDiagnosticsTruncated(err) {
		output = ""
	}
	phase, detail := remoteGitSeedDetail(output)
	text := strings.ToLower(strings.Join(detail, "\n"))
	contains := func(patterns ...string) bool {
		for _, pattern := range patterns {
			if strings.Contains(text, pattern) {
				return true
			}
		}
		return false
	}
	reason := "unknown"
	switch {
	case phase == "prerequisite":
		reason = "missing-git"
	case contains("authentication failed", "could not read username", "terminal prompts disabled", "permission denied (publickey", "access denied"):
		reason = "authentication/access"
	case contains("could not resolve host", "could not resolve hostname", "name or service not known", "temporary failure in name resolution"):
		reason = "dns"
	case contains("ssl certificate problem", "certificate verify failed", "server certificate verification failed", "ssl connect error", "schannel:"):
		reason = "tls"
	case contains("no space left on device", "disk quota exceeded", "read-only file system", "text file busy"):
		reason = "storage"
	case contains("detected dubious ownership", "dubious ownership", "safe.directory"):
		reason = "ownership"
	case contains("rpc failed", "the requested url returned error", "early eof", "unexpected disconnect", "connection reset", "http/2 stream"):
		reason = "connectivity"
	case contains("failed to connect", "connection timed out", "connection refused", "network is unreachable", "couldn't connect"):
		reason = "connectivity"
	case contains("reference is not a tree", "not a valid object name", "unable to read sha1 file", "unable to read tree", "could not fetch", "from promisor remote", "missing blob", "bad object"):
		reason = "repository/objects"
	case contains("invalid path", "unable to checkout working tree", "unable to create file", "unable to write file", "cannot create directory"):
		reason = "checkout"
	case contains("does not appear to be a git repository", "not a git repository", "couldn't find remote ref", "invalid reference", "not our ref") || (contains("repository", "remote branch") && contains("not found", "does not exist")):
		reason = "repository/ref"
	case phase == "verify":
		reason = "verification"
	}
	status := "unknown"
	var exitErr *exec.ExitError
	switch {
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status = "deadline"
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		status = strconv.Itoa(exitErr.ExitCode())
	}
	fmt.Fprintf(w, "warning: remote git seed failed: phase=%s reason=%s exit=%s; %s; Git metadata was not seeded or verified\n", phase, reason, status, disposition)
	writeRemoteGitSeedDetail(w, detail)
}

// remoteGitSeedDetail returns the phase declared by the LAST fixed marker in
// the capture and the lines that follow it: the output of the command that
// actually failed. The phase field only comes from a line that is exactly one
// of the fixed markers, so the detail block cannot inject one; a remote process
// that prints a whole marker line itself is unchanged by this patch.
func remoteGitSeedDetail(output string) (string, []string) {
	phase := "unknown"
	lines := strings.Split(output, "\n")
	detailStart := 0
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case "crabbox-git-seed phase=prerequisite":
			phase = "prerequisite"
		case "crabbox-git-seed phase=prepare":
			phase = "prepare"
		case "crabbox-git-seed phase=clone":
			phase = "clone"
		case "crabbox-git-seed phase=checkout":
			phase = "checkout"
		case "crabbox-git-seed phase=verify":
			phase = "verify"
		case "crabbox-git-seed phase=origin":
			phase = "origin"
		case "crabbox-git-seed phase=publish":
			phase = "publish"
		default:
			continue
		}
		detailStart = i + 1
	}
	return phase, lines[detailStart:]
}

func writeRemoteGitSeedDetail(w io.Writer, detail []string) {
	written, emitted := 0, 0
	omitted := false
	for _, line := range detail {
		line = sanitizeRemoteGitSeedLine(line)
		if line == "" {
			continue
		}
		if emitted == gitSeedDetailLineCount || written+len(line) > gitSeedDetailByteLimit {
			omitted = true
			break
		}
		fmt.Fprintf(w, "warning: remote git seed detail (untrusted): %s\n", line)
		written += len(line)
		emitted++
	}
	if omitted {
		fmt.Fprintln(w, "warning: remote git seed detail (untrusted): ... (further detail omitted)")
	}
}

// sanitizeRemoteGitSeedLine keeps one bounded, printable line of remote output
// so a failure names itself without letting remote text control the terminal,
// forge a log line, or leak URL credentials.
func sanitizeRemoteGitSeedLine(line string) string {
	line = stripRemoteControl(line)
	line = redactRemoteURLUserinfo(line)
	line = strings.TrimSpace(line)
	if len(line) > gitSeedDetailLineLimit {
		cut := gitSeedDetailLineLimit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "...(truncated)"
	}
	return line
}

func stripRemoteControl(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// Drop terminal control characters, including ESC and the C1 range.
		case isRemoteLineFormatControl(r):
			// Drop bidi overrides and line/paragraph separators: they do not
			// move a cursor but can reorder or split a log line.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isRemoteLineFormatControl(r rune) bool {
	switch r {
	case '‎', '‏', ' ', ' ', '‪', '‫', '‬', '‭', '‮', '⁦', '⁧', '⁨', '⁩':
		return true
	}
	return false
}

// redactRemoteURLUserinfo replaces any URL userinfo (a password, or a bare
// token used as the user) with "***" while keeping the rest of the message
// readable. Only the authority is inspected, so a '@' in the path survives.
func redactRemoteURLUserinfo(line string) string {
	var b strings.Builder
	for {
		i := strings.Index(line, "://")
		if i < 0 {
			b.WriteString(line)
			return b.String()
		}
		b.WriteString(line[:i+3])
		rest := line[i+3:]
		end := strings.IndexAny(rest, " \t\r\n'\"<>")
		token, tail := rest, ""
		if end >= 0 {
			token, tail = rest[:end], rest[end:]
		}
		authority := token
		if slash := strings.IndexByte(authority, '/'); slash >= 0 {
			authority = authority[:slash]
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			token = "***" + token[at:]
		}
		b.WriteString(token)
		line = tail
	}
}

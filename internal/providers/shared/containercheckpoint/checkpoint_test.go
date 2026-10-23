package containercheckpoint

import (
	"strings"
	"testing"
)

func TestImageNameNormalizesAndBoundsRepository(t *testing.T) {
	got := ImageName("___", "sha256:ABCDEF0123456789")
	if got != "crabbox-checkpoint-checkpoint-abcdef012345" {
		t.Fatalf("punctuation-only name=%q", got)
	}
	got = ImageName(strings.Repeat("A_", 300), "sha256:ABCDEF0123456789")
	if len(got) > MaxRepositoryLength || got != strings.ToLower(got) {
		t.Fatalf("invalid repository name=%q length=%d", got, len(got))
	}
}

func TestTagNameIsDeterministicAndBounded(t *testing.T) {
	first := TagName("checkpoint", "cp-abc123")
	if first != TagName("checkpoint", "cp-abc123") {
		t.Fatal("tag name is not deterministic")
	}
	if first == TagName("checkpoint", "cp-def456") {
		t.Fatal("tag name does not separate distinct checkpoint ids")
	}
	long := TagName(strings.Repeat("A_", 300), "cp-abc123")
	if len(long) > MaxRepositoryLength || long != strings.ToLower(long) {
		t.Fatalf("invalid tag name=%q length=%d", long, len(long))
	}
}

func TestVolumeName(t *testing.T) {
	if got := VolumeName("cp_ABC 123"); got != "crabbox-cp-cp-abc-123-state" {
		t.Fatalf("volume name=%q", got)
	}
}

func TestParseImageIDIgnoresNonDigestOutput(t *testing.T) {
	got, err := ParseImageID("warning\nsha256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("image id=%q", got)
	}
}

func TestParseImageIDRejectsAmbiguousOutput(t *testing.T) {
	first := "sha256:" + strings.Repeat("a", 64)
	second := "sha256:" + strings.Repeat("b", 64)
	if _, err := ParseImageID(first + "\n" + second + "\n"); err == nil {
		t.Fatal("ambiguous output parsed as one image id")
	}
}

func TestBootableCommandDoesNotReferenceBootstrapMount(t *testing.T) {
	change := BootableCommandChange()
	if strings.Contains(change, "crabbox-bootstrap") || !strings.HasPrefix(change, "CMD ") {
		t.Fatalf("invalid checkpoint command change: %q", change)
	}
}

func TestInspectReportsMissing(t *testing.T) {
	for _, output := range []string{"no such image", "Error: image not found", "no such object", "reference does not exist"} {
		if !InspectReportsMissing(output) {
			t.Fatalf("missing-image vocabulary not recognized: %q", output)
		}
	}
	if InspectReportsMissing("permission denied") {
		t.Fatal("permission failure reported as a missing image")
	}
	if InspectReportsMissing("not found") {
		t.Fatal("the shared Docker vocabulary must stay narrow: a bare not-found is provider-specific")
	}
}

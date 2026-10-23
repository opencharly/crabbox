package cli

import "testing"

// The containerd-commit kind must be a recognised native kind, map to its
// provider, and report the image strategy. These four functions are the whole
// core wiring a record travels through on create, verify, delete and restore.
func TestContainerdCommitKindWiring(t *testing.T) {
	if !isNativeCheckpointKind(checkpointKindContainerdCommit) {
		t.Fatal("containerd-commit must be a native checkpoint kind")
	}
	if provider := checkpointProviderForKind(checkpointKindContainerdCommit); provider != "containerd" {
		t.Fatalf("containerd-commit provider=%q, want containerd", provider)
	}
	if kind := checkpointKindForProviderImage(CoordinatorImage{Kind: CheckpointKindContainerdCommit}); kind != checkpointKindContainerdCommit {
		t.Fatalf("kind from image kind=%q", kind)
	}
	if kind := checkpointKindForProviderImage(CoordinatorImage{Provider: "containerd"}); kind != checkpointKindContainerdCommit {
		t.Fatalf("kind from image provider=%q", kind)
	}
	if strategy := checkpointStrategyForKind(checkpointKindContainerdCommit); strategy != checkpointStrategyImage {
		t.Fatalf("containerd-commit strategy=%q, want image", strategy)
	}
	if exported := CheckpointKindContainerdCommit; exported != checkpointKindContainerdCommit {
		t.Fatalf("exported kind=%q differs from the core kind", exported)
	}
}

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

// The process strategy is a first-class native strategy: it selects the
// containerd-criu kind, survives normalization, and is restored as the
// strategy of that kind. These are the functions a record travels through.
func TestContainerdCRIUProcessStrategyWiring(t *testing.T) {
	if !validCheckpointStrategy(checkpointStrategyProcess) {
		t.Fatal("process must be a valid --strategy value")
	}
	if got := NormalizeCheckpointStrategy("  PROCESS "); got != checkpointStrategyProcess {
		t.Fatalf("NormalizeCheckpointStrategy(process)=%q", got)
	}
	if got := checkpointCreateStrategy("native", checkpointStrategyProcess, checkpointKindContainerdCRIU); got != checkpointStrategyProcess {
		t.Fatalf("checkpointCreateStrategy(process)=%q", got)
	}
	if !isNativeCheckpointKind(checkpointKindContainerdCRIU) {
		t.Fatal("containerd-criu must be a native checkpoint kind")
	}
	if provider := checkpointProviderForKind(checkpointKindContainerdCRIU); provider != "containerd" {
		t.Fatalf("containerd-criu provider=%q, want containerd", provider)
	}
	if kind := checkpointKindForProviderImage(CoordinatorImage{Kind: CheckpointKindContainerdCRIU}); kind != checkpointKindContainerdCRIU {
		t.Fatalf("kind from image kind=%q", kind)
	}
	if strategy := checkpointStrategyForKind(checkpointKindContainerdCRIU); strategy != checkpointStrategyProcess {
		t.Fatalf("containerd-criu strategy=%q, want process", strategy)
	}
	if exported := CheckpointKindContainerdCRIU; exported != checkpointKindContainerdCRIU {
		t.Fatalf("exported kind=%q differs from the core kind", exported)
	}
	if exported := CheckpointStrategyProcess; exported != checkpointStrategyProcess {
		t.Fatalf("exported strategy=%q differs from the core strategy", exported)
	}
}

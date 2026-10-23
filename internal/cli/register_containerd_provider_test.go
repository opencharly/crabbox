package cli_test

// Register the real containerd provider so core checkpoint strategy tests
// exercise its capability adapter rather than a stub.
import _ "github.com/openclaw/crabbox/internal/providers/containerd"

// Package containercheckpoint holds the container-image checkpoint helpers
// shared by the local-container (docker-commit) and containerd
// (containerd-commit) providers: image naming, image-id parsing, the missing-
// image vocabulary, and the bootable replacement command.
//
// One implementation is shared so the two providers cannot drift on the parts
// of the checkpoint contract that are not engine-specific.
package containercheckpoint

import (
	"fmt"
	"strings"
)

// ImagePrefix begins every Crabbox-owned checkpoint image repository.
const ImagePrefix = "crabbox-checkpoint-"

// VolumePrefix begins every Crabbox-owned checkpoint state-volume snapshot.
const VolumePrefix = "crabbox-cp-"

// MaxRepositoryLength is the OCI repository-name ceiling that bounds the
// generated names.
const MaxRepositoryLength = 255

// ImageName renders the deterministic checkpoint image tag for a committed
// image digest. Providers that receive the digest before naming the image
// (Docker) use this form; providers that must name the image at commit time
// (nerdctl) use TagName.
func ImageName(name, imageID string) string {
	shortID := strings.TrimPrefix(strings.ToLower(imageID), "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	maxNameLength := MaxRepositoryLength - len(ImagePrefix) - 1 - len(shortID)
	return ImagePrefix + NormalizeRepositoryComponent(name, maxNameLength) + "-" + shortID
}

// TagName renders a deterministic checkpoint tag when the provider requires
// the repository at capture time. The discriminator (the durable checkpoint
// id) keeps two checkpoints that share a display name distinct.
func TagName(name, discriminator string) string {
	discriminator = NormalizeRepositoryComponent(discriminator, 48)
	maxNameLength := MaxRepositoryLength - len(ImagePrefix) - 1 - len(discriminator)
	return ImagePrefix + NormalizeRepositoryComponent(name, maxNameLength) + "-" + discriminator
}

// VolumeName renders the state-volume snapshot name owned by one checkpoint.
func VolumeName(checkpointID string) string {
	return VolumePrefix + NormalizeRepositoryComponent(checkpointID, 40) + "-state"
}

// NormalizeRepositoryComponent lowercases value to the OCI repository
// alphabet, collapsing every other run to a single dash.
func NormalizeRepositoryComponent(value string, maxLength int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(min(len(value), maxLength))
	separator := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			required := 1
			if separator && out.Len() > 0 {
				required++
			}
			if out.Len()+required > maxLength {
				break
			}
			if separator && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	normalized := strings.Trim(out.String(), "-")
	if normalized == "" {
		return "checkpoint"
	}
	return normalized
}

// ParseImageID extracts the single image id from a command's output.
func ParseImageID(output string) (string, error) {
	imageID := ""
	for _, field := range strings.Fields(output) {
		if !ValidImageID(field) {
			continue
		}
		if imageID != "" && !strings.EqualFold(imageID, field) {
			return "", fmt.Errorf("ambiguous image ids in output %q", strings.TrimSpace(output))
		}
		imageID = strings.ToLower(field)
	}
	if imageID == "" {
		return "", fmt.Errorf("missing sha256 image id in output %q", strings.TrimSpace(output))
	}
	return imageID, nil
}

// ValidImageID reports whether value is a sha256 image id.
func ValidImageID(value string) bool {
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if r < '0' || r > '9' && r < 'a' || r > 'f' {
			return false
		}
	}
	return true
}

// InspectReportsMissing reports whether an image-inspect failure means the
// image is absent rather than that the operation failed.
func InspectReportsMissing(output string) bool {
	value := strings.ToLower(output)
	return strings.Contains(value, "no such image") ||
		strings.Contains(value, "no such object") ||
		strings.Contains(value, "image not found") ||
		strings.Contains(value, "reference does not exist") ||
		strings.Contains(value, "not found")
}

// TrimFailure bounds a command's failure text for an error message.
func TrimFailure(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	const max = 500
	if len(value) > max {
		return value[:max] + "..."
	}
	return value
}

// BootableCommandChange is the Dockerfile CMD instruction that replaces the
// lease bootstrap command in a committed checkpoint image. A checkpoint is a
// base image, not a lease: it must start something that stays up without the
// /crabbox inputs mount.
func BootableCommandChange() string {
	return `CMD ["/bin/sh","-c","while :; do sleep 3600; done"]`
}

// FirstValue returns the first non-blank value.
func FirstValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

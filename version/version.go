// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package version

import (
	_ "embed"
	"fmt"
	"strings"
)

var (
	// GitCommit is the git commit that was compiled. Filled in by the linker;
	// see the LDFLAGS in the Makefile.
	GitCommit string

	// fullVersion is the next version number that will be released, updated
	// after every release. It must conform to the format expected by
	// github.com/hashicorp/go-version. A pre-release marker may be appended
	// (e.g. "-dev", "-beta", "-rc1"); its absence means a final release.
	//
	//go:embed VERSION
	fullVersion string

	// Version and VersionPrerelease are fullVersion split on the first "-".
	Version, VersionPrerelease, _ = strings.Cut(strings.TrimSpace(fullVersion), "-")

	// VersionMetadata is the semver build metadata. See
	// https://semver.org/#spec-item-10
	VersionMetadata = ""

	// BuildDate is the date/time of the build. This is the HEAD commit's date
	// rather than the wall clock, so that builds are reproducible.
	BuildDate string = "1970-01-01T00:00:01Z"
)

// GetHumanVersion composes the parts of the version in a way that's suitable
// for displaying to humans.
func GetHumanVersion() string {
	version := Version
	release := VersionPrerelease
	metadata := VersionMetadata

	if release != "" {
		version += fmt.Sprintf("-%s", release)
	}

	if metadata != "" {
		version += fmt.Sprintf("+%s", metadata)
	}

	// Strip off any single quotes added by the git information.
	return strings.ReplaceAll(version, "'", "")
}

package plugin

import (
	"fmt"
	"strconv"
	"strings"
)

// Minimal semver support for plugin update checks. Plugins ship versions as bare semver in
// plugin.yml (`1.3.0`) and release tags as `v<semver>` (`v1.3.0`); see goclawkit
// docs/sdk-spec.md "Releasing a plugin". We need only: parse a (possibly v-prefixed) MAJOR.
// MINOR.PATCH, and order two of them. Pre-release/build metadata (the `-rc1`, `+meta` tails)
// is intentionally NOT supported: a release tag for distribution is a plain MAJOR.MINOR.PATCH,
// and an update check should not offer a pre-release as "newer". A tag carrying such a tail is
// simply not parsed (treated as not-a-release), which is the safe default for "is there a
// newer STABLE release."

// semver is a parsed MAJOR.MINOR.PATCH.
type semver struct{ major, minor, patch int }

// parseSemver parses "1.3.0" or "v1.3.0" into a semver. It rejects anything that is not exactly
// three non-negative integer components (so "1.3", "1.3.0-rc1", "latest", "" all fail), which
// is what makes a pre-release tag fall through as "not a stable release".
func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("not semver: %q", s)
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("not semver: %q", s)
		}
		switch i {
		case 0:
			v.major = n
		case 1:
			v.minor = n
		case 2:
			v.patch = n
		}
	}
	return v, nil
}

// less reports whether a orders before b.
func (a semver) less(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

func (a semver) String() string { return fmt.Sprintf("%d.%d.%d", a.major, a.minor, a.patch) }

// latestSemverTag picks the highest stable v<semver> tag from a set of tag names, returning the
// original tag string (with its `v`) and its parsed value. Tags that are not exactly v<semver>
// (branches, pre-releases, junk) are ignored. ok is false if none qualify.
func latestSemverTag(tags []string) (tag string, ver semver, ok bool) {
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if !strings.HasPrefix(t, "v") {
			continue // release tags are v-prefixed by convention; bare ones are not releases
		}
		v, err := parseSemver(t)
		if err != nil {
			continue
		}
		if !ok || ver.less(v) {
			tag, ver, ok = t, v, true
		}
	}
	return tag, ver, ok
}

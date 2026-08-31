package plugin

import (
	"strconv"
	"strings"
)

// Version comparison, so that "is there a newer release of this plugin" has one
// answer rather than one per caller.
//
// Manifests are validated against strict SemVer, which means the ordering is
// fully specified and worth implementing exactly: comparing version strings
// lexically would call 0.10.0 older than 0.9.0, and an update that never
// offered itself would look identical to a plugin nobody had released.

// CompareVersions orders two SemVer strings by precedence, returning a negative
// number when left is older, zero when the two rank equally and a positive
// number when left is newer.
//
// Build metadata is ignored, as SemVer requires: two versions differing only in
// what follows a "+" are the same release built twice. A string that is not
// SemVer sorts below every version that is, so a plugin whose recorded version
// is unparseable is offered the release rather than silently kept.
func CompareVersions(left, right string) int {
	leftParsed, leftOK := parseVersion(left)
	rightParsed, rightOK := parseVersion(right)
	switch {
	case !leftOK && !rightOK:
		return strings.Compare(left, right)
	case !leftOK:
		return -1
	case !rightOK:
		return 1
	}
	return leftParsed.compare(rightParsed)
}

// IsNewerVersion reports whether candidate supersedes current.
func IsNewerVersion(candidate, current string) bool {
	return CompareVersions(candidate, current) > 0
}

type version struct {
	release [3]uint64
	// pre holds the dot-separated pre-release identifiers. An empty slice means
	// a final release, which outranks every pre-release of the same numbers.
	pre []string
}

func parseVersion(raw string) (version, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if !semverPattern.MatchString(raw) {
		return version{}, false
	}
	if plus := strings.IndexByte(raw, '+'); plus >= 0 {
		raw = raw[:plus]
	}
	var parsed version
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		parsed.pre = strings.Split(raw[dash+1:], ".")
		raw = raw[:dash]
	}
	for index, part := range strings.SplitN(raw, ".", 3) {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version{}, false
		}
		parsed.release[index] = number
	}
	return parsed, true
}

func (v version) compare(other version) int {
	for index := range v.release {
		if v.release[index] != other.release[index] {
			if v.release[index] < other.release[index] {
				return -1
			}
			return 1
		}
	}

	// "1.0.0-rc.1" precedes "1.0.0". Having no pre-release identifiers at all is
	// what makes a release final.
	switch {
	case len(v.pre) == 0 && len(other.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1
	case len(other.pre) == 0:
		return -1
	}

	for index := 0; index < len(v.pre) && index < len(other.pre); index++ {
		if result := comparePreRelease(v.pre[index], other.pre[index]); result != 0 {
			return result
		}
	}
	// Everything shared is equal, so the version with more identifiers wins.
	switch {
	case len(v.pre) < len(other.pre):
		return -1
	case len(v.pre) > len(other.pre):
		return 1
	}
	return 0
}

// comparePreRelease orders one pre-release identifier. Numeric identifiers
// compare as numbers, which is what puts "rc.11" after "rc.2" rather than
// before it, and they always rank below alphanumeric ones.
func comparePreRelease(left, right string) int {
	leftNumber, leftErr := strconv.ParseUint(left, 10, 64)
	rightNumber, rightErr := strconv.ParseUint(right, 10, 64)
	leftNumeric, rightNumeric := leftErr == nil, rightErr == nil
	switch {
	case leftNumeric && rightNumeric:
		switch {
		case leftNumber < rightNumber:
			return -1
		case leftNumber > rightNumber:
			return 1
		}
		return 0
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	}
	return strings.Compare(left, right)
}

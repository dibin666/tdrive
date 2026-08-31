package plugin

import "testing"

func TestCompareVersionsOrdersReleases(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.99.99", 1},
		// The reason this function exists rather than a string comparison.
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "1.0.10", -1},
		// Build metadata is not part of precedence.
		{"1.2.3+build.9", "1.2.3+build.1", 0},
		// A pre-release precedes the release it leads to.
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.11", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha", 1},
		// Numeric identifiers rank below alphanumeric ones.
		{"1.0.0-1", "1.0.0-alpha", -1},
		// A leading v is tolerated, since release tags carry one.
		{"v1.2.0", "1.1.0", 1},
	}
	for _, tc := range cases {
		got := CompareVersions(tc.left, tc.right)
		if sign(got) != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", tc.left, tc.right, got, tc.want)
		}
	}
}

// A version tdrive cannot parse must not be treated as up to date, or a plugin
// installed from a build with a malformed version would never be offered one
// that is correct.
func TestCompareVersionsSortsUnparseableBelowSemver(t *testing.T) {
	if CompareVersions("dev", "1.0.0") >= 0 {
		t.Error("an unparseable version ranked at or above a real one")
	}
	if !IsNewerVersion("1.0.0", "dev") {
		t.Error("a real version did not supersede an unparseable one")
	}
	if CompareVersions("dev", "dev") != 0 {
		t.Error("two identical unparseable versions did not compare equal")
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

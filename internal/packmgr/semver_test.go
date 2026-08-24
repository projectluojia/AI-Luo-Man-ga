package packmgr_test

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

func TestParseVersionAcceptsValidSemver(t *testing.T) {
	valid := []string{
		"0.0.0",
		"0.0.1",
		"1.2.3",
		"10.20.30",
		"1.2.3-alpha",
		"1.2.3-alpha.1",
		"1.2.3-rc.1+build.5",
		"1.2.3+build",
		"1.0.0-0.3.7",
	}
	for _, text := range valid {
		version, err := packmgr.ParseVersion(text)
		if err != nil {
			t.Fatalf("ParseVersion(%q) error = %v, want nil", text, err)
		}
		if version.String() == "" {
			t.Fatalf("ParseVersion(%q) produced empty String", text)
		}
	}
}

func TestParseVersionRejectsInvalidSemver(t *testing.T) {
	invalid := []string{
		"",
		"1",
		"1.2",
		"1.2.3.4",
		"v1.2.3",
		"1.2.x",
		"01.2.3",
		"1.02.3",
		"1.2.03",
		"1.2.3-",
		"1.2.3+",
		"1.2.3-01",
		"1.2.3-alpha..1",
		"1.2.3-alpha_beta",
		"1.2.3-alpha+",
		"1.2.3+",
	}
	for _, text := range invalid {
		if _, err := packmgr.ParseVersion(text); err == nil {
			t.Fatalf("ParseVersion(%q) error = nil, want error", text)
		}
	}
}

func TestCompareVersionsFollowsSemverPrecedence(t *testing.T) {
	// 相邻对：前者 < 后者。
	ascending := [][2]string{
		{"0.0.1", "0.0.2"},
		{"0.0.2", "0.1.0"},
		{"0.1.0", "1.0.0"},
		{"1.0.0", "1.0.1"},
		{"1.0.1", "1.1.0"},
		{"1.1.0", "2.0.0"},
		{"1.0.0-alpha", "1.0.0"},
		{"1.0.0-alpha", "1.0.0-alpha.1"},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta"},
		{"1.0.0-alpha.beta", "1.0.0-beta"},
		{"1.0.0-beta", "1.0.0-beta.2"},
		{"1.0.0-beta.2", "1.0.0-beta.11"},
		{"1.0.0-beta.11", "1.0.0-rc.1"},
		{"1.0.0-rc.1", "1.0.0"},
		{"1.0.0-1", "1.0.0-alpha"}, // 数字标识符 < 字母数字标识符
	}
	for _, pair := range ascending {
		left := packmgr.MustParseVersion(pair[0])
		right := packmgr.MustParseVersion(pair[1])
		if compared := packmgr.CompareVersions(left, right); compared >= 0 {
			t.Fatalf("CompareVersions(%q, %q) = %d, want < 0", pair[0], pair[1], compared)
		}
		if compared := packmgr.CompareVersions(right, left); compared <= 0 {
			t.Fatalf("CompareVersions(%q, %q) = %d, want > 0", pair[1], pair[0], compared)
		}
	}
}

func TestCompareVersionsIgnoresBuildMetadata(t *testing.T) {
	plain := packmgr.MustParseVersion("1.2.3")
	withBuild := packmgr.MustParseVersion("1.2.3+build.7")
	if compared := packmgr.CompareVersions(plain, withBuild); compared != 0 {
		t.Fatalf("build metadata must not affect precedence: %d", compared)
	}
}

func TestConstraintMatchesExact(t *testing.T) {
	constraint := packmgr.MustParseConstraint("1.2.3")
	matches(t, constraint, "1.2.3", true)
	matches(t, constraint, "1.2.4", false)
	matches(t, constraint, "1.2.2", false)
	matches(t, constraint, "2.0.0", false)
	// 预发布不匹配裸精确约束。
	matches(t, constraint, "1.2.3-alpha", false)
}

func TestConstraintMatchesCaret(t *testing.T) {
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "1.2.0", true)
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "1.9.9", true)
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "1.2.0-alpha", false)
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "1.1.9", false)
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "2.0.0", false)

	// caret 0.x 特例。
	matches(t, packmgr.MustParseConstraint("^0.2.3"), "0.2.3", true)
	matches(t, packmgr.MustParseConstraint("^0.2.3"), "0.2.9", true)
	matches(t, packmgr.MustParseConstraint("^0.2.3"), "0.3.0", false)
	matches(t, packmgr.MustParseConstraint("^0.2.3"), "0.2.2", false)
	matches(t, packmgr.MustParseConstraint("^0.0.3"), "0.0.3", true)
	matches(t, packmgr.MustParseConstraint("^0.0.3"), "0.0.4", false)
}

func TestConstraintMatchesRange(t *testing.T) {
	constraint := packmgr.MustParseConstraint(">=1.0.0,<2.0.0")
	matches(t, constraint, "1.0.0", true)
	matches(t, constraint, "1.5.0", true)
	matches(t, constraint, "2.0.0", false)
	matches(t, constraint, "0.9.9", false)

	lowerBound := packmgr.MustParseConstraint(">=1.2.3")
	matches(t, lowerBound, "1.2.3", true)
	matches(t, lowerBound, "5.0.0", true)
	matches(t, lowerBound, "1.2.2", false)

	upperBound := packmgr.MustParseConstraint("<2.0.0")
	matches(t, upperBound, "1.9.9", true)
	matches(t, upperBound, "2.0.0", false)
}

func TestConstraintPreReleaseGuard(t *testing.T) {
	// 约束未显式携带预发布时，预发布候选不匹配（semver 规范）。
	matches(t, packmgr.MustParseConstraint("^1.2.0"), "1.2.3-beta", false)
	matches(t, packmgr.MustParseConstraint(">=1.0.0,<2.0.0"), "1.2.0-beta", false)
	// 约束显式携带预发布时，范围内预发布候选可匹配（Masterminds 标准语义）。
	matches(t, packmgr.MustParseConstraint("^1.2.3-beta"), "1.2.3-beta", true)
	matches(t, packmgr.MustParseConstraint("^1.2.3-beta"), "1.2.3-beta.1", true)
	matches(t, packmgr.MustParseConstraint("^1.2.3-beta"), "1.5.0-beta", true)
	// 正式版不受守卫影响。
	matches(t, packmgr.MustParseConstraint(">=1.0.0-beta,<2.0.0"), "1.2.0", true)
}

func TestParseConstraintRejectsInvalid(t *testing.T) {
	// 仅真正非法的约束（Masterminds 标准语义接受 ^1.2、1.2.x 等合法简写）。
	invalid := []string{
		"",
		"^",
		"a.b.c",
		">=1.0.0,",
	}
	for _, text := range invalid {
		if _, err := packmgr.ParseConstraint(text); err == nil {
			t.Fatalf("ParseConstraint(%q) error = nil, want error", text)
		}
	}
}

func TestConstraintStringReturnsRaw(t *testing.T) {
	constraint := packmgr.MustParseConstraint(">=1.0.0,<2.0.0")
	if constraint.String() != ">=1.0.0,<2.0.0" {
		t.Fatalf("Constraint.String() = %q, want raw text", constraint.String())
	}
}

// matches 断言约束对指定版本的匹配结果。
func matches(t *testing.T, constraint packmgr.Constraint, versionText string, want bool) {
	t.Helper()
	version := packmgr.MustParseVersion(versionText)
	if got := constraint.Matches(version); got != want {
		t.Fatalf("Constraint(%q).Matches(%q) = %v, want %v", constraint.String(), versionText, got, want)
	}
}

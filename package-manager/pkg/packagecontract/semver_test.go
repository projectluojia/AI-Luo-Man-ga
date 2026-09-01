package packagecontract_test

import (
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
)

func mustVersion(t *testing.T, text string) packagecontract.Version {
	t.Helper()
	version, err := packagecontract.ParseVersion(text)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func mustConstraint(t *testing.T, text string) packagecontract.Constraint {
	t.Helper()
	constraint, err := packagecontract.ParseConstraint(text)
	if err != nil {
		t.Fatal(err)
	}
	return constraint
}

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
		version, err := packagecontract.ParseVersion(text)
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
		if _, err := packagecontract.ParseVersion(text); err == nil {
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
		left := mustVersion(t, pair[0])
		right := mustVersion(t, pair[1])
		if compared := packagecontract.CompareVersions(left, right); compared >= 0 {
			t.Fatalf("CompareVersions(%q, %q) = %d, want < 0", pair[0], pair[1], compared)
		}
		if compared := packagecontract.CompareVersions(right, left); compared <= 0 {
			t.Fatalf("CompareVersions(%q, %q) = %d, want > 0", pair[1], pair[0], compared)
		}
	}
}

func TestCompareVersionsIgnoresBuildMetadata(t *testing.T) {
	plain := mustVersion(t, "1.2.3")
	withBuild := mustVersion(t, "1.2.3+build.7")
	if compared := packagecontract.CompareVersions(plain, withBuild); compared != 0 {
		t.Fatalf("build metadata must not affect precedence: %d", compared)
	}
}

func TestConstraintMatchesExact(t *testing.T) {
	constraint := mustConstraint(t, "1.2.3")
	matches(t, constraint, "1.2.3", true)
	matches(t, constraint, "1.2.4", false)
	matches(t, constraint, "1.2.2", false)
	matches(t, constraint, "2.0.0", false)
	// 预发布不匹配裸精确约束。
	matches(t, constraint, "1.2.3-alpha", false)
}

func TestConstraintMatchesCaret(t *testing.T) {
	matches(t, mustConstraint(t, "^1.2.0"), "1.2.0", true)
	matches(t, mustConstraint(t, "^1.2.0"), "1.9.9", true)
	matches(t, mustConstraint(t, "^1.2.0"), "1.2.0-alpha", false)
	matches(t, mustConstraint(t, "^1.2.0"), "1.1.9", false)
	matches(t, mustConstraint(t, "^1.2.0"), "2.0.0", false)

	// caret 0.x 特例。
	matches(t, mustConstraint(t, "^0.2.3"), "0.2.3", true)
	matches(t, mustConstraint(t, "^0.2.3"), "0.2.9", true)
	matches(t, mustConstraint(t, "^0.2.3"), "0.3.0", false)
	matches(t, mustConstraint(t, "^0.2.3"), "0.2.2", false)
	matches(t, mustConstraint(t, "^0.0.3"), "0.0.3", true)
	matches(t, mustConstraint(t, "^0.0.3"), "0.0.4", false)
}

func TestConstraintMatchesRange(t *testing.T) {
	constraint := mustConstraint(t, ">=1.0.0,<2.0.0")
	matches(t, constraint, "1.0.0", true)
	matches(t, constraint, "1.5.0", true)
	matches(t, constraint, "2.0.0", false)
	matches(t, constraint, "0.9.9", false)

	lowerBound := mustConstraint(t, ">=1.2.3")
	matches(t, lowerBound, "1.2.3", true)
	matches(t, lowerBound, "5.0.0", true)
	matches(t, lowerBound, "1.2.2", false)

	upperBound := mustConstraint(t, "<2.0.0")
	matches(t, upperBound, "1.9.9", true)
	matches(t, upperBound, "2.0.0", false)
}

func TestConstraintPreReleaseGuard(t *testing.T) {
	// 约束未显式携带预发布时，预发布候选不匹配（semver 规范）。
	matches(t, mustConstraint(t, "^1.2.0"), "1.2.3-beta", false)
	matches(t, mustConstraint(t, ">=1.0.0,<2.0.0"), "1.2.0-beta", false)
	// 约束显式携带预发布时，范围内预发布候选可匹配（Masterminds 标准语义）。
	matches(t, mustConstraint(t, "^1.2.3-beta"), "1.2.3-beta", true)
	matches(t, mustConstraint(t, "^1.2.3-beta"), "1.2.3-beta.1", true)
	matches(t, mustConstraint(t, "^1.2.3-beta"), "1.5.0-beta", true)
	// 正式版不受守卫影响。
	matches(t, mustConstraint(t, ">=1.0.0-beta,<2.0.0"), "1.2.0", true)
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
		if _, err := packagecontract.ParseConstraint(text); err == nil {
			t.Fatalf("ParseConstraint(%q) error = nil, want error", text)
		}
	}
}

func TestConstraintStringReturnsRaw(t *testing.T) {
	constraint := mustConstraint(t, ">=1.0.0,<2.0.0")
	if constraint.String() != ">=1.0.0,<2.0.0" {
		t.Fatalf("Constraint.String() = %q, want raw text", constraint.String())
	}
}

// matches 断言约束对指定版本的匹配结果。
func matches(t *testing.T, constraint packagecontract.Constraint, versionText string, want bool) {
	t.Helper()
	version := mustVersion(t, versionText)
	if got := constraint.Matches(version); got != want {
		t.Fatalf("Constraint(%q).Matches(%q) = %v, want %v", constraint.String(), versionText, got, want)
	}
}

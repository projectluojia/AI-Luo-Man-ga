package packmgr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 包版本契约的地基：semver 2.0 解析、比较与约束。
// 版本号是包的公共契约（对宿主、App 配置、lock、审计可见），实现只接受严格
// X.Y.Z（可选 -prerelease 与 +build）；比较遵循 semver 2.0 优先级规则
// （构建元数据不参与优先级）。约束支持精确、caret（^）与逗号分隔的范围
// 比较器（>=、>、<=、<），预发布版本按 npm 规则只匹配显式携带预发布的约束。

var (
	errInvalidVersion    = errors.New("invalid semantic version")
	errInvalidConstraint = errors.New("invalid version constraint")
)

// Version 是解析后的语义化版本。
type Version struct {
	Major      uint64
	Minor      uint64
	Patch      uint64
	PreRelease []string
	Build      []string
}

// ParseVersion 解析严格 semver 2.0 版本号；非法输入返回 errInvalidVersion。
func ParseVersion(text string) (Version, error) {
	var version Version
	text = strings.TrimSpace(text)
	if text == "" {
		return Version{}, errInvalidVersion
	}
	if plus := strings.IndexByte(text, '+'); plus >= 0 {
		build, err := parseIdentifiers(text[plus+1:])
		if err != nil {
			return Version{}, errInvalidVersion
		}
		version.Build = build
		text = text[:plus]
	}
	if dash := strings.IndexByte(text, '-'); dash >= 0 {
		pre, err := parseIdentifiers(text[dash+1:])
		if err != nil {
			return Version{}, errInvalidVersion
		}
		version.PreRelease = pre
		text = text[:dash]
	}
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return Version{}, errInvalidVersion
	}
	numbers := [3]uint64{}
	for index, part := range parts {
		number, err := parseNumericIdentifier(part)
		if err != nil {
			return Version{}, errInvalidVersion
		}
		numbers[index] = number
	}
	version.Major, version.Minor, version.Patch = numbers[0], numbers[1], numbers[2]
	return version, nil
}

// MustParseVersion 在版本号确定合法时使用；非法输入 panic（仅限常量/测试）。
func MustParseVersion(text string) Version {
	version, err := ParseVersion(text)
	if err != nil {
		panic(fmt.Sprintf("invalid semantic version %q: %v", text, err))
	}
	return version
}

// String 以规范形式输出版本号（保留预发布，丢弃构建元数据）。
func (v Version) String() string {
	text := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.PreRelease) > 0 {
		text += "-" + strings.Join(v.PreRelease, ".")
	}
	return text
}

// CompareVersions 按 semver 2.0 优先级比较；a<b 返回 -1，a==b 返回 0，a>b 返回 1。
func CompareVersions(a, b Version) int {
	if a.Major != b.Major {
		return compareUint64(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return compareUint64(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return compareUint64(a.Patch, b.Patch)
	}
	return comparePreRelease(a.PreRelease, b.PreRelease)
}

func compareUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePreRelease 比较预发布标识列表：无预发布 > 有预发布；数字标识符按数值、
// 字母数字标识符按 ASCII 字典序；全部相等时较短列表优先级更高。
func comparePreRelease(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for index := 0; index < limit; index++ {
		if compared := compareIdentifier(a[index], b[index]); compared != 0 {
			return compared
		}
	}
	return compareUint64(uint64(len(a)), uint64(len(b)))
}

// compareIdentifier 比较单个预发布标识符：数字 < 字母数字。
func compareIdentifier(a, b string) int {
	aNumeric, bNumeric := allDigits(a), allDigits(b)
	switch {
	case aNumeric && bNumeric:
		aValue, _ := strconv.ParseUint(a, 10, 64)
		bValue, _ := strconv.ParseUint(b, 10, 64)
		return compareUint64(aValue, bValue)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	default:
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		default:
			return 0
		}
	}
}

// versionComparator 是单个版本比较器（op 与基准版本）。
type versionComparator struct {
	op      string // "="、">="、">"、"<="、"<"
	version Version
}

// Constraint 是版本约束：comparators 之间为 AND 语义；raw 保留原始文本供展示。
type Constraint struct {
	comparators []versionComparator
	raw         string
}

// ParseConstraint 解析版本约束。
// 支持形式：精确 "1.2.3"；caret "^1.2.0"（>=1.2.0 且 <2.0.0，含 0.x 特例）；
// 范围 ">=1.0.0,<2.0.0"（比较器之间 AND）。非法输入返回 errInvalidConstraint。
func ParseConstraint(text string) (Constraint, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Constraint{}, errInvalidConstraint
	}
	constraint := Constraint{raw: text}
	seen := make(map[string]struct{})
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Constraint{}, errInvalidConstraint
		}
		operator, versionText := splitComparator(part)
		version, err := ParseVersion(versionText)
		if err != nil {
			return Constraint{}, errInvalidConstraint
		}
		switch operator {
		case "^":
			lower := versionComparator{op: ">=", version: version}
			upper := versionComparator{op: "<", version: caretUpperBound(version)}
			if err := constraint.appendUnique(seen, lower); err != nil {
				return Constraint{}, err
			}
			if err := constraint.appendUnique(seen, upper); err != nil {
				return Constraint{}, err
			}
		case "", "=", ">=", ">", "<=", "<":
			if operator == "" {
				operator = "="
			}
			comparator := versionComparator{op: operator, version: version}
			if err := constraint.appendUnique(seen, comparator); err != nil {
				return Constraint{}, err
			}
		default:
			return Constraint{}, errInvalidConstraint
		}
	}
	if len(constraint.comparators) == 0 {
		return Constraint{}, errInvalidConstraint
	}
	return constraint, nil
}

func (c *Constraint) appendUnique(seen map[string]struct{}, comparator versionComparator) error {
	key := comparator.op + " " + comparator.version.String()
	if _, exists := seen[key]; exists {
		return errInvalidConstraint
	}
	seen[key] = struct{}{}
	c.comparators = append(c.comparators, comparator)
	return nil
}

// MustParseConstraint 在约束确定合法时使用；非法输入 panic（仅限常量/测试）。
func MustParseConstraint(text string) Constraint {
	constraint, err := ParseConstraint(text)
	if err != nil {
		panic(fmt.Sprintf("invalid version constraint %q: %v", text, err))
	}
	return constraint
}

// String 返回原始约束文本。
func (c Constraint) String() string { return c.raw }

// Matches 判断版本是否满足约束；预发布版本只匹配显式携带预发布的约束
// （npm 规则：约束中必须存在与候选同 M.m.p 且带预发布的比较器）。
func (c Constraint) Matches(version Version) bool {
	if len(c.comparators) == 0 {
		return false
	}
	if !c.allowsPreRelease(version) {
		return false
	}
	for _, comparator := range c.comparators {
		if !comparator.matches(version) {
			return false
		}
	}
	return true
}

func (c Constraint) allowsPreRelease(version Version) bool {
	if len(version.PreRelease) == 0 {
		return true
	}
	for _, comparator := range c.comparators {
		candidate := comparator.version
		if len(candidate.PreRelease) > 0 && candidate.Major == version.Major &&
			candidate.Minor == version.Minor && candidate.Patch == version.Patch {
			return true
		}
	}
	return false
}

func (comparator versionComparator) matches(version Version) bool {
	compared := CompareVersions(version, comparator.version)
	switch comparator.op {
	case "=":
		return compared == 0
	case ">=":
		return compared >= 0
	case ">":
		return compared > 0
	case "<=":
		return compared <= 0
	case "<":
		return compared < 0
	default:
		return false
	}
}

// caretUpperBound 计算 caret 约束的排他上界：^1.2.3 -> 2.0.0；^0.2.3 -> 0.3.0；
// ^0.0.3 -> 0.0.4。
func caretUpperBound(version Version) Version {
	switch {
	case version.Major > 0:
		return Version{Major: version.Major + 1}
	case version.Minor > 0:
		return Version{Minor: version.Minor + 1}
	default:
		return Version{Patch: version.Patch + 1}
	}
}

// splitComparator 拆分比较器前缀与版本文本；无前缀时 operator 为空串。
func splitComparator(part string) (operator, versionText string) {
	for _, candidate := range []string{">=", "<=", "^", ">", "<", "="} {
		if strings.HasPrefix(part, candidate) {
			return candidate, strings.TrimSpace(strings.TrimPrefix(part, candidate))
		}
	}
	return "", strings.TrimSpace(part)
}

func parseNumericIdentifier(part string) (uint64, error) {
	if part == "" || !allDigits(part) {
		return 0, errInvalidVersion
	}
	if len(part) > 1 && part[0] == '0' {
		return 0, errInvalidVersion
	}
	return strconv.ParseUint(part, 10, 64)
}

// parseIdentifiers 解析点分隔的预发布/构建标识符。
func parseIdentifiers(text string) ([]string, error) {
	if text == "" {
		return nil, errInvalidVersion
	}
	parts := strings.Split(text, ".")
	for _, part := range parts {
		if part == "" {
			return nil, errInvalidVersion
		}
		for _, character := range part {
			if !isIdentifierCharacter(byte(character)) {
				return nil, errInvalidVersion
			}
		}
		if allDigits(part) && len(part) > 1 && part[0] == '0' {
			return nil, errInvalidVersion
		}
	}
	return parts, nil
}

func isIdentifierCharacter(character byte) bool {
	return character >= '0' && character <= '9' ||
		character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character == '-'
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, character := range text {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

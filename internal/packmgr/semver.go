package packmgr

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
)

// Version 是语义化版本 2.0 的轻量包装，底层委托 Masterminds/semver（Go 生态
// 事实标准 semver 库），消除手写解析/比较/约束的易错逻辑。
type Version struct {
	v *semver.Version
}

// ParseVersion 严格解析 major.minor.patch 三部分（无 v 前缀，与 Masterminds
// StrictNewVersion 一致）。"1.2.3-beta" 等预发布与构建元数据合法。
func ParseVersion(text string) (Version, error) {
	v, err := semver.StrictNewVersion(text)
	if err != nil {
		return Version{}, fmt.Errorf("packmgr: 版本 %q 不合法: %w", text, err)
	}
	return Version{v: v}, nil
}

// MustParseVersion 与 ParseVersion 相同，解析失败时 panic（测试辅助）。
func MustParseVersion(text string) Version {
	v, err := ParseVersion(text)
	if err != nil {
		panic(err)
	}
	return v
}

// String 返回版本字符串（如 "1.2.3"）。
func (v Version) String() string {
	return v.v.String()
}

// CompareVersions 按 semver 2.0 优先级比较两版本：a < b 返回 -1，相等 0，a > b 1。
func CompareVersions(a, b Version) int {
	return a.v.Compare(b.v)
}

// Constraint 是版本约束（如 "^1.2.3"、"~1.2"、"1.x"、">=1.0.0 <2.0.0"），
// 底层委托 Masterminds/semver 约束解析器（支持复杂约束与交集）。
type Constraint struct {
	c   *semver.Constraints
	raw string
}

// ParseConstraint 解析版本约束（^/~/=/</>/<=/>=/!=/x 通配/区间/|| 组合）。
func ParseConstraint(text string) (Constraint, error) {
	c, err := semver.NewConstraint(text)
	if err != nil {
		return Constraint{}, fmt.Errorf("packmgr: 约束 %q 不合法: %w", text, err)
	}
	return Constraint{c: c, raw: text}, nil
}

// MustParseConstraint 解析约束，失败时 panic（测试辅助）。
func MustParseConstraint(text string) Constraint {
	c, err := ParseConstraint(text)
	if err != nil {
		panic(err)
	}
	return c
}

// String 返回约束原始文本。
func (c Constraint) String() string {
	return c.raw
}

// Matches 判断版本是否满足约束。
func (c Constraint) Matches(version Version) bool {
	return c.c.Check(version.v)
}

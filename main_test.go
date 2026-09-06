package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestMaintenanceBackupValidateAndRestoreCommands(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "source.db")
	backup := filepath.Join(directory, "backup.db")
	restored := filepath.Join(directory, "restored.db")
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	handled, err := runMaintenanceCommand([]string{"backup", "--database", database, "--destination", backup}, output)
	if err != nil || !handled || !strings.Contains(output.String(), "完整性校验") {
		t.Fatalf("backup handled=%t output=%q err=%v", handled, output.String(), err)
	}
	output.Reset()
	handled, err = runMaintenanceCommand([]string{"validate-backup", "--backup", backup}, output)
	if err != nil || !handled {
		t.Fatalf("validate handled=%t err=%v", handled, err)
	}
	output.Reset()
	handled, err = runMaintenanceCommand([]string{"restore", "--backup", backup, "--destination", restored}, output)
	if err != nil || !handled {
		t.Fatalf("restore handled=%t err=%v", handled, err)
	}
	opened, err := sqlite.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
}

func TestMaintenanceCommandsRejectAmbiguousOrDestructiveTargets(t *testing.T) {
	handled, err := runMaintenanceCommand([]string{"backup", "--database", "relative.db", "--destination", "backup.db"}, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("relative paths handled=%t err=%v", handled, err)
	}
	handled, err = runMaintenanceCommand([]string{"unknown"}, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("unknown command handled=%t err=%v", handled, err)
	}
}

func TestMaintenanceIdentityBindIsIdempotent(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	arguments := []string{
		"identity-bind",
		"--database", database,
		"--user", "user-qq-1",
		"--app", "test-app",
		"--platform", "qq",
		"--space", "space-qq-1",
		"--platform-user", "openid-qq-1",
	}
	output := &bytes.Buffer{}
	handled, err := runMaintenanceCommand(arguments, output)
	if err != nil || !handled || !strings.Contains(output.String(), "身份开通完成") {
		t.Fatalf("first provision handled=%t output=%q err=%v", handled, output.String(), err)
	}
	// 幂等重放：同一命令重复执行必须成功且不改变结果。
	output.Reset()
	handled, err = runMaintenanceCommand(arguments, output)
	if err != nil || !handled {
		t.Fatalf("replayed provision handled=%t err=%v", handled, err)
	}
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolved, err := identity.NewService(store).ResolveIdentity(t.Context(), "test-app", "qq", "space-qq-1", "openid-qq-1")
	if err != nil {
		t.Fatalf("resolve bound identity: %v", err)
	}
	if resolved.UserID != "user-qq-1" {
		t.Fatalf("bound user=%q want user-qq-1", resolved.UserID)
	}
}

func TestMaintenanceIdentityBindRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"identity-bind"}, // 缺必填参数
		{"identity-bind", "--database", "relative.db", "--user", "user-1"},              // 相对路径
		{"identity-bind", "--database", "x.db", "--user", "user-1", "--platform", "qq"}, // 绑定参数不全
	}
	for _, arguments := range cases {
		handled, err := runMaintenanceCommand(arguments, &bytes.Buffer{})
		if !handled || err == nil {
			t.Fatalf("arguments=%v handled=%t err=%v", arguments, handled, err)
		}
	}
}

func TestMaintenanceIdentityUnbind(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	bind := []string{"identity-bind", "--database", database, "--user", "user-qq-1", "--app", "test-app", "--platform", "qq", "--space", "private", "--platform-user", "openid-qq-1"}
	if handled, err := runMaintenanceCommand(bind, &bytes.Buffer{}); err != nil || !handled {
		t.Fatalf("bind handled=%t err=%v", handled, err)
	}
	unbind := []string{"identity-unbind", "--database", database, "--app", "test-app", "--platform", "qq", "--space", "private", "--platform-user", "openid-qq-1"}
	output := &bytes.Buffer{}
	if handled, err := runMaintenanceCommand(unbind, output); err != nil || !handled || !strings.Contains(output.String(), "身份解绑完成") {
		t.Fatalf("unbind handled=%t output=%q err=%v", handled, output.String(), err)
	}
	store, err := sqlite.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := identity.NewService(store).ResolveIdentity(t.Context(), "test-app", "qq", "private", "openid-qq-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("resolve after unbind error=%v, want ErrNotFound", err)
	}
	// 再次解绑：身份不存在返回 ErrNotFound，命令仍明确报错。
	handled, err := runMaintenanceCommand(unbind, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("second unbind handled=%t err=%v", handled, err)
	}
}

func TestMaintenanceIdentityBindRejectsConflictingBinding(t *testing.T) {
	database := filepath.Join(t.TempDir(), "identity.db")
	base := []string{"identity-bind", "--database", database, "--app", "test-app", "--platform", "qq", "--space", "space-qq-1", "--platform-user", "openid-qq-1"}
	if handled, err := runMaintenanceCommand(append(append([]string{}, base...), "--user", "user-qq-1"), &bytes.Buffer{}); err != nil || !handled {
		t.Fatalf("first bind handled=%t err=%v", handled, err)
	}
	// 同一外部身份绑定到另一个内部用户必须被拒绝。
	handled, err := runMaintenanceCommand(append(append([]string{}, base...), "--user", "user-qq-2"), &bytes.Buffer{})
	if !handled || err == nil || !strings.Contains(err.Error(), "身份开通失败") {
		t.Fatalf("conflicting bind handled=%t err=%v", handled, err)
	}
}

func TestRuntimeHostCommandRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"runtime-host"}, // 缺必填参数
		{"runtime-host", "--install-root", "relative", "--address", "127.0.0.1:0"},   // 相对安装目录
		{"runtime-host", "--install-root", t.TempDir(), "--address", "0.0.0.0:7000"}, // 非 loopback 监听
	}
	for _, arguments := range cases {
		handled, err := runMaintenanceCommand(arguments, &bytes.Buffer{})
		if !handled || err == nil {
			t.Fatalf("arguments=%v handled=%t err=%v", arguments, handled, err)
		}
	}
}

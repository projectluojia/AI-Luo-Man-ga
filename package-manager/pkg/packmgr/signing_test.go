package packmgr_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	packageiotest "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio/testutil"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
)

// writeSignedTarball 从隔离进程源包产出带发布方签名的 tarball，返回
// tarball 路径与签名者公钥的十六进制编码。
func writeSignedTarball(t *testing.T, id string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := hex.EncodeToString(publicKey)
	t.Setenv("AILUO_SIGNING_KEY", hex.EncodeToString(privateKey.Seed()))
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, id, "1.0.0", packagecontract.ModeIsolated, "app", nil)
	manifestBytes, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest packagecontract.Manifest
	if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	tarball, err := packmgr.PackFromSource(context.Background(), source, t.TempDir(), manifest, manifestBytes, privateKey)
	if err != nil {
		t.Fatalf("PackFromSource: %v", err)
	}
	return tarball, signer
}

// TestInstallPropagatesSignatureAcrossPathRewrite 验证签名信任链的路径无关性：
// 发布物 lock 的签名在安装器重写绝对路径、固化进程规格后依然对载荷有效。
func TestInstallPropagatesSignatureAcrossPathRewrite(t *testing.T) {
	tarball, signer := writeSignedTarball(t, "signed.pkg")
	record, err := packmgr.Install(context.Background(), packageiotest.TempDir(t), tarball)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if record.Lock.Signature == nil {
		t.Fatal("installed lock lost the publisher signature")
	}
	if record.Lock.Signature.Signer != signer {
		t.Fatalf("signer = %q, want %q", record.Lock.Signature.Signer, signer)
	}
	if err := packagecontract.Verify(record.Lock, map[string]struct{}{signer: {}}); err != nil {
		t.Fatalf("verify installed lock: %v", err)
	}
}

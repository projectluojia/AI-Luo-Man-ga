package campus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/builtin"
)

// hostedVersion 是 campus hosted 包的内置版本。
const hostedVersion = "1.0.0"

// campusWASMDigest 在包初始化时锁定嵌入工件的 SHA-256。
var campusWASMDigest = func() string {
	sum := sha256.Sum256(builtin.CampusWASM)
	return hex.EncodeToString(sum[:])
}()

// Manifest 返回 campus hosted 包的内置清单；digest 锁定嵌入工件，
// 保证装载的永远是随内核发布的构建产物。
func Manifest() loader.Manifest {
	return loader.Manifest{
		ID: ServiceID, Version: hostedVersion, Mode: loader.ModeHosted,
		LockedDigest: campusWASMDigest, Pin: true,
	}
}

// ReadArtifact 返回嵌入工件并校验清单 digest 一致，防止构建产物与清单漂移。
func ReadArtifact(_ context.Context, manifest loader.Manifest) ([]byte, error) {
	if manifest.ID != ServiceID || manifest.LockedDigest != campusWASMDigest {
		return nil, loader.ErrDescribeMismatch
	}
	return builtin.CampusWASM, nil
}

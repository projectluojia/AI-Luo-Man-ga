package packmgr

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// SigningKeyFromEnv 从 AILUO_SIGNING_KEY 读取 Ed25519 签名密钥（64 字节种子 +
// 公钥，或 32 字节种子的十六进制编码）。变量未设置返回 nil（不签名）；设置了
// 但非法直接报错——签名入口不静默降级为未签名发布。
func SigningKeyFromEnv() (ed25519.PrivateKey, error) {
	value := strings.TrimSpace(os.Getenv("AILUO_SIGNING_KEY"))
	if value == "" {
		return nil, nil
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("AILUO_SIGNING_KEY 不是合法十六进制: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	default:
		return nil, fmt.Errorf("AILUO_SIGNING_KEY 长度 %d 字节，需要 %d 或 %d 字节",
			len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// signLock 若签名密钥非空则对 lock 签名；未配置签名密钥时原样返回 lock——
// isolated 包的信任由消费方按部署策略强制。
func signLock(lock packagecontract.Lock, key ed25519.PrivateKey) (packagecontract.Lock, error) {
	if key == nil {
		return lock, nil
	}
	signature, err := packagecontract.Sign(lock, key)
	if err != nil {
		return packagecontract.Lock{}, fmt.Errorf("签署包 lock 失败: %w", err)
	}
	lock.Signature = &signature
	return lock, nil
}

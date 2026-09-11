package packagecontract

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidSignature 表示签名载荷无法验证：签名者公钥、签名值或载荷格式非法，
// 或签名与内容不匹配。调用方区分"格式非法"与"不匹配"没有意义——两者都意味着
// 该 lock 不可信。
var ErrInvalidSignature = errors.New("package lock signature is invalid")

// SignaturePayload 是签名覆盖的确切内容：清单摘要与按 ComponentID 排序的工件
// 摘要。不含路径与进程规格——它们由安装器按部署环境派生并各自经 SHA-256 锁定，
// 因此签名可原样穿越 pack（相对路径）→ install（绝对路径）→ rebase。
type SignaturePayload struct {
	SchemaVersion  string           `json:"schema_version"`
	PackageID      string           `json:"package_id"`
	PackageVersion string           `json:"package_version"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Artifacts      []SignedArtifact `json:"artifacts"`
}

// SignedArtifact 是签名载荷中的一个工件身份：只绑定组件与摘要，不含路径。
type SignedArtifact struct {
	ComponentID string `json:"component_id"`
	SHA256      string `json:"sha256"`
}

// SignaturePayload 从 lock 提取签名载荷。Artifacts 按 ComponentID 排序：打包与
// 安装不必共享同一工件顺序，签名对顺序稳定。
func (l Lock) SignaturePayload() (SignaturePayload, error) {
	if len(l.Artifacts) == 0 {
		return SignaturePayload{}, fmt.Errorf("%w: lock has no artifacts", ErrInvalidSignature)
	}
	payload := SignaturePayload{
		SchemaVersion:  l.SchemaVersion,
		PackageID:      l.PackageID,
		PackageVersion: l.PackageVersion,
		ManifestSHA256: l.ManifestSHA256,
		Artifacts:      make([]SignedArtifact, 0, len(l.Artifacts)),
	}
	for _, artifact := range l.Artifacts {
		payload.Artifacts = append(payload.Artifacts, SignedArtifact{
			ComponentID: artifact.ComponentID, SHA256: artifact.SHA256,
		})
	}
	sort.Slice(payload.Artifacts, func(i, j int) bool {
		return payload.Artifacts[i].ComponentID < payload.Artifacts[j].ComponentID
	})
	return payload, nil
}

// Sign 用 Ed25519 私钥签署 lock 的签名载荷，返回可写入 lock.Signature 的签名。
func Sign(lock Lock, privateKey ed25519.PrivateKey) (Signature, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return Signature{}, fmt.Errorf("%w: private key size %d", ErrInvalidSignature, len(privateKey))
	}
	payload, err := lock.SignaturePayload()
	if err != nil {
		return Signature{}, err
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return Signature{}, ErrInvalidSignature
	}
	value := ed25519.Sign(privateKey, payloadBytes(payload))
	return Signature{
		Signer: hex.EncodeToString(publicKey), Value: hex.EncodeToString(value),
	}, nil
}

// Verify 校验 lock 携带的签名对载荷有效，并确认签名者是 expectedSigner 之一。
// expectedSigner 为部署方信任的 Ed25519 公钥十六进制集合；空集合直接拒绝。
func Verify(lock Lock, expectedSigners map[string]struct{}) error {
	if lock.Signature == nil {
		return fmt.Errorf("%w: lock is unsigned", ErrInvalidSignature)
	}
	if len(expectedSigners) == 0 {
		return fmt.Errorf("%w: no trusted signers configured", ErrInvalidSignature)
	}
	if _, trusted := expectedSigners[lock.Signature.Signer]; !trusted {
		return fmt.Errorf("%w: signer %q is not trusted", ErrInvalidSignature, lock.Signature.Signer)
	}
	signer, err := hex.DecodeString(lock.Signature.Signer)
	if err != nil || len(signer) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: signer key", ErrInvalidSignature)
	}
	value, err := hex.DecodeString(lock.Signature.Value)
	if err != nil || len(value) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature value", ErrInvalidSignature)
	}
	payload, err := lock.SignaturePayload()
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(signer), payloadBytes(payload), value) {
		return ErrInvalidSignature
	}
	return nil
}

// payloadBytes 把载荷序列化为确定的字节串（JSON，key 有序由结构体字段序保证）。
// 先求 SHA-256 再让 Ed25519 签名，使载荷与签名字节长度解耦。
func payloadBytes(payload SignaturePayload) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 结构体只含 string 与固定形状的切片，Marshal 不会失败；失败即编程错误。
		panic(fmt.Sprintf("marshal signature payload: %v", err))
	}
	digest := sha256.Sum256(encoded)
	return digest[:]
}

package ecard

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// ParseAES256Key 接受 32 字节原始密钥或 64 字符 hex；失败时不把输入写入错误。
// 恰好 32 字节的输入按原始密钥处理，即使边界字节是空白也不裁剪。
func ParseAES256Key(raw []byte) ([]byte, error) {
	if len(raw) == AES256KeySize {
		key := make([]byte, AES256KeySize)
		copy(key, raw)
		return key, nil
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == AES256KeySize {
		key := make([]byte, AES256KeySize)
		copy(key, trimmed)
		return key, nil
	}
	if len(trimmed) == AES256KeySize*2 && isHex(trimmed) {
		key := make([]byte, AES256KeySize)
		if _, err := hex.Decode(key, trimmed); err != nil {
			return nil, ErrKeyInvalid
		}
		return key, nil
	}
	return nil, ErrKeyInvalid
}

func encryptMaterial(key []byte, appID, userID, kind string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if len(key) != AES256KeySize {
		return nil, nil, ErrKeyInvalid
	}
	if len(plaintext) == 0 || len(plaintext) > MaxMaterialBytes {
		return nil, nil, ErrInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, ErrKeyInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, ErrKeyInvalid
	}
	if aead.NonceSize() != GCMNonceSize {
		return nil, nil, fmt.Errorf("%w: unexpected nonce size", ErrKeyInvalid)
	}
	nonce = make([]byte, GCMNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate ecard nonce: %w", err)
	}
	ciphertext = aead.Seal(nil, nonce, plaintext, associatedData(appID, userID, kind))
	if len(ciphertext) < MinCiphertext || len(ciphertext) > MaxCiphertext {
		clearBytes(ciphertext)
		return nil, nil, ErrInvalid
	}
	return nonce, ciphertext, nil
}

func decryptMaterial(key, nonce, ciphertext []byte, appID, userID, kind string) ([]byte, error) {
	if len(key) != AES256KeySize || len(nonce) != GCMNonceSize || len(ciphertext) < MinCiphertext || len(ciphertext) > MaxCiphertext {
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrKeyInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrKeyInvalid
	}
	plain, err := aead.Open(nil, nonce, ciphertext, associatedData(appID, userID, kind))
	if err != nil {
		return nil, ErrInvalid
	}
	return plain, nil
}

func associatedData(appID, userID, kind string) []byte {
	return []byte(appID + "\x00" + userID + "\x00" + kind)
}

func clearBytes(values ...[]byte) {
	for _, value := range values {
		for i := range value {
			value[i] = 0
		}
	}
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func isHex(raw []byte) bool {
	if len(raw) == 0 || len(raw)%2 != 0 {
		return false
	}
	for _, b := range raw {
		if !unicode.Is(unicode.ASCII_Hex_Digit, rune(b)) {
			return false
		}
	}
	return true
}

// Package tgclient 封装 gotd：账号在线管理、扫码登录、对话同步。
package tgclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

// cipherFor 用 MASTER_KEY 派生 AES-256-GCM。
func cipherFor(masterKey string) (cipher.AEAD, error) {
	k := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encrypt(masterKey string, plain []byte) ([]byte, error) {
	aead, err := cipherFor(masterKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, nil), nil
}

func decrypt(masterKey string, data []byte) ([]byte, error) {
	aead, err := cipherFor(masterKey)
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, body := data[:aead.NonceSize()], data[aead.NonceSize():]
	return aead.Open(nil, nonce, body, nil)
}

package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// EncryptedVault is the explicit headless alternative to the desktop OS keychain.
// Its 32-byte master key is supplied by the host environment, never the config export.
type EncryptedVault struct {
	dir  string
	aead cipher.AEAD
	mu   sync.Mutex
}

func NewEncryptedVault(dir string, key []byte) (*EncryptedVault, error) {
	if len(key) != 32 {
		return nil, errors.New("无界面凭据库需要 32 字节主密钥")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &EncryptedVault{dir: dir, aead: aead}, nil
}
func (v *EncryptedVault) filename(k string) string {
	hash := sha256.Sum256([]byte(k))
	return filepath.Join(v.dir, hex.EncodeToString(hash[:])+".secret")
}
func (v *EncryptedVault) Get(k string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	raw, err := os.ReadFile(v.filename(k))
	if err != nil {
		return "", err
	}
	n := v.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("凭据数据无效")
	}
	plain, err := v.aead.Open(nil, raw[:n], raw[n:], []byte(k))
	if err != nil {
		return "", errors.New("凭据主密钥错误或内容损坏")
	}
	return string(plain), nil
}
func (v *EncryptedVault) Set(k, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	raw := v.aead.Seal(nonce, nonce, []byte(value), []byte(k))
	f, err := os.CreateTemp(v.dir, ".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), v.filename(k))
}
func (v *EncryptedVault) Delete(k string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	err := os.Remove(v.filename(k))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

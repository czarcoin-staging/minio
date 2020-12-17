// MinIO Cloud Storage, (C) 2015, 2016, 2017, 2018 MinIO, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package crypto

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/storj/minio/cmd/logger"
	sha256 "github.com/minio/sha256-simd"
	"github.com/minio/sio"
)

// Context is a list of key-value pairs cryptographically
// associated with a certain object.
type Context map[string]string

// WriteTo writes the context in a canonical from to w.
// It returns the number of bytes and the first error
// encounter during writing to w, if any.
//
// WriteTo sorts the context keys and writes the sorted
// key-value pairs as canonical JSON object to w.
//
// Note that neither keys nor values are escaped for JSON.
func (c Context) WriteTo(w io.Writer) (n int64, err error) {
	sortedKeys := make(sort.StringSlice, 0, len(c))
	for k := range c {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Sort(sortedKeys)

	nn, err := io.WriteString(w, "{")
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)
	for i, k := range sortedKeys {
		s := fmt.Sprintf("\"%s\":\"%s\",", k, c[k])
		if i == len(sortedKeys)-1 {
			s = s[:len(s)-1] // remove last ','
		}

		nn, err = io.WriteString(w, s)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}
	nn, err = io.WriteString(w, "}")
	return n + int64(nn), err
}

// AppendTo appends the context in a canonical from to dst.
//
// AppendTo sorts the context keys and writes the sorted
// key-value pairs as canonical JSON object to w.
//
// Note that neither keys nor values are escaped for JSON.
func (c Context) AppendTo(dst []byte) (output []byte) {
	if len(c) == 0 {
		return append(dst, '{', '}')
	}

	// out should not escape.
	out := bytes.NewBuffer(dst)

	// No need to copy+sort
	if len(c) == 1 {
		for k, v := range c {
			out.WriteString(`{"`)
			out.WriteString(k)
			out.WriteString(`":"`)
			out.WriteString(v)
			out.WriteString(`"}`)
		}
		return out.Bytes()
	}

	sortedKeys := make([]string, 0, len(c))
	for k := range c {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	out.WriteByte('{')
	for i, k := range sortedKeys {
		out.WriteByte('"')
		out.WriteString(k)
		out.WriteString(`":"`)
		out.WriteString(c[k])
		out.WriteByte('"')
		if i < len(sortedKeys)-1 {
			out.WriteByte(',')
		}
	}
	out.WriteByte('}')
	return out.Bytes()
}

// KMS represents an active and authenticted connection
// to a Key-Management-Service. It supports generating
// data key generation and unsealing of KMS-generated
// data keys.
type KMS interface {
	// DefaultKeyID returns the default master key ID. It should be
	// used for SSE-S3 and whenever a S3 client requests SSE-KMS but
	// does not specify an explicit SSE-KMS key ID.
	DefaultKeyID() string

	// CreateKey creates a new master key with the given key ID
	// at the KMS.
	CreateKey(keyID string) error

	// GenerateKey generates a new random data key using
	// the master key referenced by the keyID. It returns
	// the plaintext key and the sealed plaintext key
	// on success.
	//
	// The context is cryptographically bound to the
	// generated key. The same context must be provided
	// again to unseal the generated key.
	GenerateKey(keyID string, context Context) (key [32]byte, sealedKey []byte, err error)

	// UnsealKey unseals the sealedKey using the master key
	// referenced by the keyID. The provided context must
	// match the context used to generate the sealed key.
	UnsealKey(keyID string, sealedKey []byte, context Context) (key [32]byte, err error)

	// Info returns descriptive information about the KMS,
	// like the default key ID and authentication method.
	Info() KMSInfo
}

type masterKeyKMS struct {
	keyID     string
	masterKey [32]byte
}

// KMSInfo contains some describing information about
// the KMS.
type KMSInfo struct {
	Endpoints []string
	Name      string
	AuthType  string
}

// NewMasterKey returns a basic KMS implementation from a single 256 bit master key.
//
// The KMS accepts any keyID but binds the keyID and context cryptographically
// to the generated keys.
func NewMasterKey(keyID string, key [32]byte) KMS { return &masterKeyKMS{keyID: keyID, masterKey: key} }

func (kms *masterKeyKMS) DefaultKeyID() string {
	return kms.keyID
}

func (kms *masterKeyKMS) CreateKey(keyID string) error {
	return errors.New("crypto: creating keys is not supported by a static master key")
}

func (kms *masterKeyKMS) GenerateKey(keyID string, ctx Context) (key [32]byte, sealedKey []byte, err error) {
	if _, err = io.ReadFull(rand.Reader, key[:]); err != nil {
		logger.CriticalIf(context.Background(), errOutOfEntropy)
	}

	var (
		buffer     bytes.Buffer
		derivedKey = kms.deriveKey(keyID, ctx)
	)
	if n, err := sio.Encrypt(&buffer, bytes.NewReader(key[:]), sio.Config{Key: derivedKey[:]}); err != nil || n != 64 {
		logger.CriticalIf(context.Background(), errors.New("KMS: unable to encrypt data key"))
	}
	sealedKey = buffer.Bytes()
	return key, sealedKey, nil
}

// KMS is configured directly using master key
func (kms *masterKeyKMS) Info() (info KMSInfo) {
	return KMSInfo{
		Endpoints: []string{},
		Name:      "",
		AuthType:  "master-key",
	}
}

func (kms *masterKeyKMS) UnsealKey(keyID string, sealedKey []byte, ctx Context) (key [32]byte, err error) {
	var (
		derivedKey = kms.deriveKey(keyID, ctx)
	)
	out, err := sio.DecryptBuffer(key[:0], sealedKey, sio.Config{Key: derivedKey[:]})
	if err != nil || len(out) != 32 {
		return key, err // TODO(aead): upgrade sio to use sio.Error
	}
	return key, nil
}

func (kms *masterKeyKMS) deriveKey(keyID string, context Context) (key [32]byte) {
	if context == nil {
		context = Context{}
	}
	mac := hmac.New(sha256.New, kms.masterKey[:])
	mac.Write([]byte(keyID))
	mac.Write(context.AppendTo(make([]byte, 0, 128)))
	mac.Sum(key[:0])
	return key
}

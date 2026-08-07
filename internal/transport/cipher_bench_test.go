// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// The SSH transport's bulk cost is its AEAD. Which one is fastest depends on
// whether the CPU has AES instructions, so this measures the two we offer.
func BenchmarkTransportAEAD(b *testing.B) {
	payload := make([]byte, 32*1024) // one SSH max packet
	_, _ = rand.Read(payload)
	nonce := make([]byte, 12)
	dst := make([]byte, 0, len(payload)+64)

	b.Run("chacha20-poly1305", func(b *testing.B) {
		key := make([]byte, chacha20poly1305.KeySize)
		_, _ = rand.Read(key)
		aead, err := chacha20poly1305.New(key)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(payload)))
		for b.Loop() {
			_ = aead.Seal(dst[:0], nonce, payload, nil)
		}
	})

	for _, bits := range []int{128, 256} {
		b.Run("aes-gcm-"+itoa(bits), func(b *testing.B) {
			key := make([]byte, bits/8)
			_, _ = rand.Read(key)
			block, err := aes.NewCipher(key)
			if err != nil {
				b.Fatal(err)
			}
			aead, err := cipher.NewGCM(block)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				_ = aead.Seal(dst[:0], nonce, payload, nil)
			}
		})
	}
}

func itoa(n int) string {
	if n == 128 {
		return "128"
	}
	return "256"
}

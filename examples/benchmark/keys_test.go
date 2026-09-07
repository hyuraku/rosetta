package main

import "testing"

// TestKeyForIndex_Deterministic covers KNOWN_ISSUES.md R18: the same index
// must always produce the same key, so preload and a later read agree.
func TestKeyForIndex_Deterministic(t *testing.T) {
	for _, keySize := range []int{16, 32, 64} {
		for _, index := range []int{0, 1, 42, 999} {
			a := keyForIndex(index, keySize)
			b := keyForIndex(index, keySize)
			if a != b {
				t.Errorf("keyForIndex(%d, %d) not deterministic: %q vs %q", index, keySize, a, b)
			}
		}
	}
}

// TestKeyForIndex_Length covers the "length = keySize" requirement: the
// benchmark's -key-size flag should be honored exactly, byte for byte.
func TestKeyForIndex_Length(t *testing.T) {
	for _, keySize := range []int{minKeySize(999999), 16, 32, 64, 128} {
		got := keyForIndex(12345, keySize)
		if len(got) != keySize {
			t.Errorf("keyForIndex(12345, %d): len = %d, want %d (key %q)", keySize, len(got), keySize, got)
		}
	}
}

// TestKeyForIndex_DifferentIndicesDiffer checks that distinct indices produce
// distinct keys when keySize is at least minKeySize for the larger index —
// the case validateConfig is supposed to guarantee in production use.
func TestKeyForIndex_DifferentIndicesDiffer(t *testing.T) {
	const keySize = 24
	seen := make(map[string]int)
	for i := 0; i < 1000; i++ {
		if minKeySize(i) > keySize {
			t.Fatalf("test bug: keySize %d too small for index %d (needs %d)", keySize, i, minKeySize(i))
		}
		key := keyForIndex(i, keySize)
		if prev, exists := seen[key]; exists {
			t.Fatalf("keyForIndex(%d, %d) == keyForIndex(%d, %d) == %q, want distinct keys", i, keySize, prev, keySize, key)
		}
		seen[key] = i
	}
}

// TestKeyForIndex_PreloadReadAgree is the direct regression test for R18's
// bug: the key preload writes for an index and the key a read requests for
// that same index must be byte-identical.
func TestKeyForIndex_PreloadReadAgree(t *testing.T) {
	const keySize = 20
	for index := 0; index < 50; index++ {
		preloadKey := keyForIndex(index, keySize)
		readKey := keyForIndex(index, keySize)
		if preloadKey != readKey {
			t.Fatalf("index %d: preload key %q != read key %q", index, preloadKey, readKey)
		}
	}
}

func TestMinKeySize(t *testing.T) {
	tests := []struct {
		maxIndex int
		want     int
	}{
		{0, len("key-") + len("0") + 1},
		{9, len("key-") + 1 + 1},
		{10, len("key-") + 2 + 1},
		{99, len("key-") + 2 + 1},
		{100, len("key-") + 3 + 1},
		{-5, len("key-") + 1 + 1}, // negative clamps to 0
	}
	for _, tt := range tests {
		if got := minKeySize(tt.maxIndex); got != tt.want {
			t.Errorf("minKeySize(%d) = %d, want %d", tt.maxIndex, got, tt.want)
		}
	}
}

// TestKeyForIndex_TruncatesWhenTooSmall documents the (validated-against, not
// panicking) degenerate case: a keySize below minKeySize truncates rather
// than erroring, which validateConfig exists to prevent in normal use.
func TestKeyForIndex_TruncatesWhenTooSmall(t *testing.T) {
	got := keyForIndex(123456, 4)
	if len(got) != 4 {
		t.Fatalf("keyForIndex(123456, 4): len = %d, want 4", len(got))
	}
	if got != "key-" {
		t.Fatalf("keyForIndex(123456, 4) = %q, want %q (prefix truncation)", got, "key-")
	}
}

package main

import (
	"fmt"
	"strconv"
)

// keyPrefix is the fixed, human-readable prefix every generated key starts
// with, e.g. "key-42-".
const keyPrefix = "key-"

// keyPadChar fills a key out to keySize bytes. It is deliberately not a digit
// or "-", so padding can never be misread as encoding a different index.
const keyPadChar = 'x'

// keyForIndex deterministically derives a key of exactly keySize bytes from
// an index: the same index always produces the same key, byte for byte.
//
// This is what makes the read workload actually hit something
// (KNOWN_ISSUES.md R18): populateInitialData, worker's read path and worker's
// write path all call keyForIndex with the same (index, keySize) pair for a
// given logical key, so a GET always requests exactly the string a PUT wrote
// for that index. The previous generateKey drew a fresh random suffix on
// every call — even with the same numeric seed, the *rand.Rand stream had
// advanced between the preload call and the later read call, so the two
// strings almost never matched and GETs were close to always 404.
//
// Callers must validate keySize against minKeySize first — validateConfig
// does, before any key is generated. keyForIndex itself does not validate: if
// keySize is too small to hold the prefix plus every digit of index, the
// index is silently truncated, which can make two different indices collide
// on the same key.
func keyForIndex(index, keySize int) string {
	base := fmt.Sprintf("%s%d-", keyPrefix, index)
	if len(base) >= keySize {
		return base[:keySize]
	}

	padded := make([]byte, keySize)
	copy(padded, base)
	for i := len(base); i < keySize; i++ {
		padded[i] = keyPadChar
	}
	return string(padded)
}

// minKeySize returns the smallest -key-size (in bytes) that can hold
// keyPrefix plus every digit of maxIndex plus the separating "-", without
// truncating (and thereby risking a collision). maxIndex is typically the
// largest index the benchmark plans to generate a key for — see
// maxPlannedIndex.
func minKeySize(maxIndex int) int {
	if maxIndex < 0 {
		maxIndex = 0
	}
	return len(keyPrefix) + len(strconv.Itoa(maxIndex)) + 1
}

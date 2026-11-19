package identity

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// The BIP-39 English word list. Tieroute uses only the list and its
// checksum, not BIP-39's seed derivation (§6.1).
//
//go:embed bip39-english.txt
var wordListText string

var (
	wordList  = strings.Fields(wordListText)
	wordIndex = func() map[string]int {
		m := make(map[string]int, len(wordList))
		for i, w := range wordList {
			m[w] = i
		}
		return m
	}()
)

// CodeSize is the size of a recovery code: 16 random bytes (§6.1).
const CodeSize = 16

// WordCount is the number of words a recovery code is written as.
const WordCount = 12

// ErrWords reports recovery words that aren't a valid code.
var ErrWords = errors.New("identity: the recovery words aren't valid")

// EncodeWords writes 16 bytes as 12 BIP-39 words: 128 bits and a 4-bit
// checksum, 11 bits a word (§6.1).
func EncodeWords(code []byte) ([]string, error) {
	if len(code) != CodeSize {
		return nil, fmt.Errorf("identity: a recovery code is %d bytes", CodeSize)
	}
	sum := sha256.Sum256(code)
	bits := append(append([]byte{}, code...), sum[0]) // 128 bits, then the top 4 of the checksum byte
	words := make([]string, WordCount)
	for i := range WordCount {
		var v int
		for j := range 11 {
			bit := i*11 + j
			v = v<<1 | int(bits[bit/8]>>(7-bit%8)&1)
		}
		words[i] = wordList[v]
	}
	return words, nil
}

// DecodeWords reads 12 recovery words back into 16 bytes and checks the
// checksum, which catches typos (§6.1). It trims space and ignores case.
func DecodeWords(words []string) ([]byte, error) {
	if len(words) != WordCount {
		return nil, fmt.Errorf("%w: want %d words, got %d", ErrWords, WordCount, len(words))
	}
	bits := make([]byte, 17)
	for i, w := range words {
		v, ok := wordIndex[strings.ToLower(strings.TrimSpace(w))]
		if !ok {
			return nil, fmt.Errorf("%w: word %d isn't on the list", ErrWords, i+1)
		}
		for j := range 11 {
			if v>>(10-j)&1 == 1 {
				bit := i*11 + j
				bits[bit/8] |= 1 << (7 - bit%8)
			}
		}
	}
	code := bits[:CodeSize]
	sum := sha256.Sum256(code)
	if sum[0]>>4 != bits[16]>>4 {
		return nil, fmt.Errorf("%w: the checksum doesn't match", ErrWords)
	}
	return append([]byte{}, code...), nil
}

// ParseWords splits text on spaces and decodes it as recovery words.
func ParseWords(text string) ([]byte, error) {
	return DecodeWords(strings.Fields(text))
}

// IsWord reports whether w is on the word list, for checking words one at
// a time as the user types them.
func IsWord(w string) bool {
	_, ok := wordIndex[strings.ToLower(strings.TrimSpace(w))]
	return ok
}

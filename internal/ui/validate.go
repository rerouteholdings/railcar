package ui

import (
	"errors"
	"strconv"
	"strings"
)

// normalizeHandle trims a handle as typed, drops a leading @, and lowercases
// it.
func normalizeHandle(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// checkHandle checks a normalized handle against the handle syntax (§5.4): a
// lowercase DNS name with at least two labels, each 1 to 63 characters from
// [a-z0-9-] that don't start or end with -, and 253 characters at most.
func checkHandle(h string) error {
	if h == "" {
		return errors.New("Enter a handle, such as kat.chat.example.com")
	}
	if len(h) > 253 {
		return errors.New("A handle can be 253 characters at most")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return errors.New("A handle is a domain name with at least one dot")
	}
	for _, l := range labels {
		if err := checkLabel(l); err != nil {
			return err
		}
	}
	return nil
}

func checkLabel(l string) error {
	if l == "" {
		return errors.New("A domain name can't have an empty part")
	}
	if len(l) > 63 {
		return errors.New("Each part of a domain name can be 63 characters at most")
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return errors.New("A part of a domain name can't start or end with a hyphen")
	}
	for _, r := range l {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return errors.New("Use only letters, digits, hyphens, and dots")
		}
	}
	return nil
}

// normalizeHomeserver trims a homeserver name as typed, drops an https://
// prefix and a trailing slash, and lowercases it.
func normalizeHomeserver(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimSuffix(s, "/")
}

// checkHomeserver checks a normalized homeserver name: a domain name, with an
// optional port for testing.
func checkHomeserver(s string) error {
	if s == "" {
		return errors.New("Enter your homeserver, such as chat.example.com")
	}
	host, port, hasPort := strings.Cut(s, ":")
	if hasPort {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("The port must be a number from 1 to 65535")
		}
	}
	if host == "localhost" {
		return nil
	}
	if err := checkHandle(host); err != nil {
		return errors.New("Enter the homeserver's domain name, such as chat.example.com")
	}
	return nil
}

// splitWords splits a recovery code as typed into lowercase words.
func splitWords(s string) []string {
	return strings.Fields(strings.ToLower(s))
}

// checkRecoveryWords checks that a recovery code has 12 words of letters
// only (§6.1). The backend checks the word list and the checksum.
func checkRecoveryWords(s string) error {
	words := splitWords(s)
	if len(words) != 12 {
		return errors.New("A recovery code has 12 words; you entered " + strconv.Itoa(len(words)))
	}
	for _, w := range words {
		for _, r := range w {
			if r < 'a' || r > 'z' {
				return errors.New("Recovery words have letters only")
			}
		}
	}
	return nil
}

// checkRequired checks that a field isn't empty.
func checkRequired(what string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("Enter " + what)
		}
		return nil
	}
}

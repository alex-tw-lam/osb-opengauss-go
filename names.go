// names.go derives every database object name and every generated secret:
// short hashes, sanitized names, quoted identifiers and literals, and
// passwords. It is pure code: no configuration, no database, no templates.

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"strings"
)

// Names holds every database object that belongs to one service instance.
type Names struct {
	Database   string
	GroupRole  string
	Tablespace string
}

const maxIdentifier = 63

var sanitizePattern = regexp.MustCompile(`[^a-z0-9_]`)

func shortHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

func sanitizeName(name string, maxLen int) string {
	cleaned := sanitizePattern.ReplaceAllString(strings.ToLower(name), "")
	if len(cleaned) > maxLen {
		cleaned = cleaned[:maxLen]
	}
	return cleaned
}

func NamesFor(instanceID, prefix, customName string) Names {
	// The budget keeps prefix + "_" + name + "_grp" within maxIdentifier.
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-5); name != "" {
		base = prefix + "_" + name
	} else {
		base = prefix + "_" + shortHash(instanceID)
	}
	return Names{Database: base, GroupRole: base + "_grp", Tablespace: base + "_ts"}
}

func UserFor(bindingID, prefix, customName string) string {
	// The budget keeps prefix + "u_" + name within maxIdentifier.
	var base string
	if name := sanitizeName(customName, maxIdentifier-len(prefix)-2); name != "" {
		base = prefix + "u_" + name
	} else {
		base = prefix + "u_" + shortHash(bindingID)
	}
	return base
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#%*+-=?@^_~"

func randomPassword() string {
	for {
		password := make([]byte, 28)
		for i := range password {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
			if err != nil {
				panic(err) // a failed system entropy source is unrecoverable
			}
			password[i] = passwordAlphabet[n.Int64()]
		}
		var hasUpper, hasLower, hasDigit, hasSpecial bool
		for _, c := range string(password) {
			switch {
			case c >= 'A' && c <= 'Z':
				hasUpper = true
			case c >= 'a' && c <= 'z':
				hasLower = true
			case c >= '0' && c <= '9':
				hasDigit = true
			default:
				hasSpecial = true
			}
		}
		if hasUpper && hasLower && hasDigit && hasSpecial {
			return string(password)
		}
	}
}

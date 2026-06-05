package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrEmptyName     = errors.New("name must not be empty")
	ErrInvalidName   = errors.New("name must be latin-only after transliteration")
	ErrEmptyWorldDir = errors.New("world dir must not be empty")
)

var nonLatin = regexp.MustCompile(`[^A-Za-z0-9_\-]`)

var translitMap = map[string]string{
	"а": "a", "б": "b", "в": "v", "г": "g", "д": "d", "е": "e", "ё": "yo",
	"ж": "zh", "з": "z", "и": "i", "й": "y", "к": "k", "л": "l", "м": "m",
	"н": "n", "о": "o", "п": "p", "р": "r", "с": "s", "т": "t", "у": "u",
	"ф": "f", "х": "kh", "ц": "ts", "ч": "ch", "ш": "sh", "щ": "sch",
	"ъ": "", "ы": "y", "ь": "", "э": "e", "ю": "yu", "я": "ya",
}

func Transliterate(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == ' ' || r == '\t':
			b.WriteByte('_')
		case r < 128:
			b.WriteRune(r)
		default:
			if mapped, ok := translitMap[string(r)]; ok {
				b.WriteString(mapped)
			} else {
				b.WriteRune('_')
			}
		}
	}
	return b.String()
}

func SanitizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrEmptyName
	}
	if hasCyrillic(name) && !looksIntentionalLatin(name) {
		converted := Transliterate(name)
		name = converted
	}
	cleaned := nonLatin.ReplaceAllString(name, "_")
	cleaned = strings.Trim(cleaned, "_-")
	if cleaned == "" {
		return "", ErrInvalidName
	}
	return strings.ToLower(cleaned), nil
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return true
		}
	}
	return false
}

func looksIntentionalLatin(s string) bool {
	return !hasCyrillic(s)
}

func ValidateWorldDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return ErrEmptyWorldDir
	}
	if hasCyrillic(dir) {
		return fmt.Errorf("world dir %q contains cyrillic characters: %w", dir, ErrInvalidName)
	}
	return nil
}

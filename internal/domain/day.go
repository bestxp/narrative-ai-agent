package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var dayLineRe = regexp.MustCompile(`(?m)^д(\d{5}):\s*(.+)$`)

type DayEntry struct {
	Number int
	Text   string
}

func ParseDays(content string) ([]DayEntry, error) {
	matches := dayLineRe.FindAllStringSubmatch(content, -1)
	if matches == nil {
		return nil, nil
	}
	out := make([]DayEntry, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, err
		}
		out = append(out, DayEntry{Number: n, Text: strings.TrimSpace(m[2])})
	}
	return out, nil
}

func FormatDay(n int, summary string) string {
	return "д" + fmt.Sprintf("%05d", n) + ": " + summary
}

func LastDay(content string) (int, bool) {
	entries, err := ParseDays(content)
	if err != nil || len(entries) == 0 {
		return 0, false
	}
	return entries[len(entries)-1].Number, true
}

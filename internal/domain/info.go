package domain

import (
	"errors"
	"regexp"
	"strings"
)

var infoHeaderRe = regexp.MustCompile(`(?m)^-\s*\[(АКТИВЕН|НЕАКТИВЕН)\]\s+(.+?)\s*$`)

type ActiveRef struct {
	Active  bool
	Pointer string
}

func ParseInfo(content string) (charRef ActiveRef, worldRef ActiveRef, err error) {
	for _, line := range strings.Split(content, "\n") {
		m := infoHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		active := m[1] == "АКТИВЕН"
		ptr := m[2]
		switch {
		case strings.HasPrefix(ptr, "characters/") && charRef.Pointer == "":
			charRef = ActiveRef{Active: active, Pointer: ptr}
		case strings.HasPrefix(ptr, "worlds/") && worldRef.Pointer == "":
			worldRef = ActiveRef{Active: active, Pointer: ptr}
		}
	}
	if charRef.Pointer == "" && worldRef.Pointer == "" {
		return charRef, worldRef, errors.New("info.md: no character or world references found")
	}
	return charRef, worldRef, nil
}

func BuildInfo(activeChar, activeWorld string, allChars, allWorlds []string) string {
	var b strings.Builder
	b.WriteString("# Lazy Multiverse — реестр\n\n")
	b.WriteString("## Активный персонаж\n")
	b.WriteString("- [АКТИВЕН] characters/" + activeChar + "\n\n")
	b.WriteString("## Активный мир\n")
	b.WriteString("- [АКТИВЕН] worlds/" + activeWorld + "\n\n")
	b.WriteString("## Персонажи\n")
	for _, c := range allChars {
		if c == activeChar {
			continue
		}
		b.WriteString("- [НЕАКТИВЕН] characters/" + c + "\n")
	}
	b.WriteString("\n## Миры\n")
	for _, w := range allWorlds {
		if w == activeWorld {
			continue
		}
		b.WriteString("- [НЕАКТИВЕН] worlds/" + w + "\n")
	}
	b.WriteString("\n## Правила (якоря — перечитать при старте сессии)\n")
	b.WriteString("- [ ] Не управляю персонажем игрока\n")
	b.WriteString("- [ ] Не спрашиваю направление сцены\n")
	b.WriteString("- [ ] Обслуживание файлов — первоочередной приоритет\n")
	b.WriteString("- [ ] INDEX POLLUTION: только patch или cat→write_file\n")
	b.WriteString("- [ ] memorise.md: д{NNNNN}, сухо, при «конец дня»\n")
	b.WriteString("- [ ] NPC знают только то, что им сказали (info-isolation)\n")
	b.WriteString("- [ ] Имена файлов — только латиница\n")
	b.WriteString("- [ ] Проверяю git push, не доверяю git commit\n")
	return b.String()
}

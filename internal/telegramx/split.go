package telegramx

import "unicode/utf8"

// MaxMessageRunes — лимит текста одного сообщения Telegram (символы).
const MaxMessageRunes = 4096

// SplitMessageRunes режет строку на части не длиннее maxRunes (по рунам).
func SplitMessageRunes(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = MaxMessageRunes
	}
	rs := []rune(s)
	if len(rs) <= maxRunes {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var out []string
	for i := 0; i < len(rs); i += maxRunes {
		end := i + maxRunes
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}

// RuneLen возвращает длину строки в символах Unicode.
func RuneLen(s string) int {
	return utf8.RuneCountInString(s)
}

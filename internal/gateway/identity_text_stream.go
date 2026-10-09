package gateway

import (
	"bufio"
	"errors"
	"io"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// A JSON string is decoded a rune at a time. Raw offsets let us remove only
// matching bytes, preserving every unrelated JSON field and escape sequence.
type identityTextSource struct {
	raw     io.ReaderAt
	span    identitySpan // contents, excluding JSON quotes
	escaped bool
}

type identityRune struct {
	char       rune
	size       int
	start, end int64
}

type identityRunes struct {
	r       *bufio.Reader
	pos     int64
	escaped bool
	pending identityRune
	hasPeek bool
}

func (s identityTextSource) runes(start int64) *identityRunes {
	return &identityRunes{r: bufio.NewReader(io.NewSectionReader(s.raw, start, s.span.end-start)), pos: start, escaped: s.escaped}
}

func identityHex(raw []byte) (rune, bool) {
	var value rune
	for _, b := range raw {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value += rune(b - '0')
		case b >= 'a' && b <= 'f':
			value += rune(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value += rune(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func (r *identityRunes) read() (identityRune, error) {
	start := r.pos
	char, size, err := r.r.ReadRune()
	if err != nil {
		return identityRune{}, err
	}
	r.pos += int64(size)
	if !r.escaped {
		return identityRune{char, size, start, r.pos}, nil
	}
	if char == '\\' {
		b, err := r.r.ReadByte()
		if err != nil {
			return identityRune{}, err
		}
		r.pos++
		switch b {
		case '"', '\\', '/':
			char = rune(b)
		case 'b':
			char = '\b'
		case 'f':
			char = '\f'
		case 'n':
			char = '\n'
		case 'r':
			char = '\r'
		case 't':
			char = '\t'
		case 'u':
			var raw [4]byte
			if _, err := io.ReadFull(r.r, raw[:]); err != nil {
				return identityRune{}, err
			}
			r.pos += 4
			var ok bool
			char, ok = identityHex(raw[:])
			if !ok {
				return identityRune{}, errors.New("invalid JSON string escape")
			}
			if char >= 0xD800 && char <= 0xDBFF {
				if next, err := r.r.Peek(6); err == nil && next[0] == '\\' && next[1] == 'u' {
					if low, ok := identityHex(next[2:]); ok && low >= 0xDC00 && low <= 0xDFFF {
						_, _ = r.r.Discard(6)
						r.pos += 6
						char = utf16.DecodeRune(char, low)
					}
				}
			}
			if char >= 0xD800 && char <= 0xDFFF {
				char = utf8.RuneError
			}
		default:
			return identityRune{}, errors.New("invalid JSON string escape")
		}
	}
	return identityRune{char, utf8.RuneLen(char), start, r.pos}, nil
}

func (r *identityRunes) next() (identityRune, error) {
	if r.hasPeek {
		r.hasPeek = false
		return r.pending, nil
	}
	return r.read()
}

func (r *identityRunes) peek() (identityRune, error) {
	if !r.hasPeek {
		var err error
		r.pending, err = r.read()
		if err != nil {
			return identityRune{}, err
		}
		r.hasPeek = true
	}
	return r.pending, nil
}

func (r *identityRunes) ReadRune() (rune, int, error) {
	value, err := r.next()
	return value.char, value.size, err
}

func identitySentenceEnd(source identityTextSource, openingEnd int64) (int64, error) {
	r := source.runes(source.span.start)
	var decoded int64
	blankLine := false
	for {
		value, err := r.next()
		if err == io.EOF {
			return source.span.end, nil
		}
		if err != nil {
			return 0, err
		}
		before := decoded
		decoded += int64(value.size)
		if before < openingEnd {
			continue
		}
		char := value.char
		if char == '\n' {
			if blankLine {
				return value.start, nil
			}
			blankLine = true
		} else if char != '\t' && char != '\r' && !unicode.Is(unicode.Zs, char) {
			blankLine = false
		}
		switch char {
		case '.', '!', '?', '。', '！', '？':
			if char == '.' {
				next, _ := r.peek()
				if unicode.IsLetter(next.char) || unicode.IsDigit(next.char) {
					continue
				}
			}
			return value.start, nil
		}
	}
}

type identityQuoteState struct{ quote, previous rune }

func (s *identityQuoteState) consume(char, next rune) {
	previous := s.previous
	s.previous = char
	if char == '\'' && unicode.IsLetter(previous) && unicode.IsLetter(next) {
		return
	}
	if char == '\'' && s.quote == 0 && unicode.IsLetter(previous) && (unicode.IsSpace(next) || next == 0) {
		return
	}
	if s.quote != 0 {
		if char == s.quote {
			s.quote = 0
		}
		return
	}
	switch char {
	case '"', '\'', '`':
		s.quote = char
	case '“':
		s.quote = '”'
	case '‘':
		s.quote = '’'
	}
}

func locateIdentityMatch(source identityTextSource, start int64, match []int, state *identityQuoteState) (identitySpan, bool, error) {
	r := source.runes(start)
	var decoded int64
	var span identitySpan
	quoted := false
	for decoded < int64(match[1]) {
		value, err := r.next()
		if err != nil {
			return span, false, err
		}
		if decoded == int64(match[0]) {
			span.start, quoted = value.start, state.quote != 0
		}
		next, _ := r.peek()
		state.consume(value.char, next.char)
		decoded += int64(value.size)
		span.end = value.end
	}
	if decoded != int64(match[1]) {
		return span, false, errors.New("invalid identity match boundary")
	}
	// Inspect beyond the sentence boundary too, so GPT-5.1 is never mistaken
	// for GPT-5 followed by sentence punctuation.
	after := source.runes(span.end)
	next, _ := after.next()
	if unicode.IsLetter(next.char) || unicode.IsDigit(next.char) || next.char == '_' || next.char == '-' {
		return span, false, nil
	}
	if next.char == '.' {
		following, _ := after.next()
		if unicode.IsLetter(following.char) || unicode.IsDigit(following.char) {
			return span, false, nil
		}
	}
	return span, !quoted, nil
}

func scanIdentityText(source identityTextSource, emit func(identitySpan) error) (bool, error) {
	opening := identityOpening.FindReaderIndex(source.runes(source.span.start))
	if opening == nil {
		return false, nil
	}
	end, err := identitySentenceEnd(source, int64(opening[1]))
	if err != nil {
		return false, err
	}
	cursor := source.span.start
	state := identityQuoteState{}
	changed := false
	for cursor < end {
		sentence := source
		sentence.span.end = end
		match := identityQualifier.FindReaderIndex(sentence.runes(cursor))
		if match == nil {
			break
		}
		span, allowed, err := locateIdentityMatch(source, cursor, match, &state)
		if err != nil {
			return false, err
		}
		if allowed {
			if err := emit(span); err != nil {
				return false, err
			}
			changed = true
		}
		cursor = span.end
	}
	return changed, nil
}

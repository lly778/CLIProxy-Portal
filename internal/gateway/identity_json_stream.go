package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

var errIdentityJSON = errors.New("invalid identity JSON payload")

type identityJSON struct {
	ctx             context.Context
	file            *os.File
	r               *bufio.Reader
	pos             int64
	depth           int
	model           string
	candidates      *os.File
	candidateWriter *bufio.Writer
	candidateBytes  int64
	patches         *os.File
	patchWriter     *bufio.Writer
	patchCount      int64
}

func (s *identityJSON) peek() (byte, error) {
	raw, err := s.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return raw[0], nil
}

func (s *identityJSON) byte() (byte, error) {
	b, err := s.r.ReadByte()
	if err == nil {
		s.pos++
	}
	return b, err
}

func (s *identityJSON) whitespace() error {
	for {
		b, err := s.peek()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			_, _ = s.byte()
		default:
			return nil
		}
	}
}

func (s *identityJSON) expect(want byte) error {
	if err := s.whitespace(); err != nil {
		return err
	}
	b, err := s.byte()
	if err != nil {
		return err
	}
	if b != want {
		return errIdentityJSON
	}
	return nil
}

func (s *identityJSON) string() (identitySpan, error) {
	if err := s.whitespace(); err != nil {
		return identitySpan{}, err
	}
	start := s.pos
	if err := s.expect('"'); err != nil {
		return identitySpan{}, err
	}
	for {
		b, err := s.byte()
		if err != nil {
			return identitySpan{}, err
		}
		switch {
		case b == '"':
			return identitySpan{start, s.pos}, nil
		case b < 0x20:
			return identitySpan{}, errIdentityJSON
		case b == '\\':
			escape, err := s.byte()
			if err != nil {
				return identitySpan{}, err
			}
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				var raw [4]byte
				for i := range raw {
					raw[i], err = s.byte()
					if err != nil {
						return identitySpan{}, err
					}
				}
				if _, ok := identityHex(raw[:]); !ok {
					return identitySpan{}, errIdentityJSON
				}
			default:
				return identitySpan{}, errIdentityJSON
			}
		}
	}
}

// Keys and routing metadata have known short values. Long unknown keys/roles
// are skipped without allocation; this is not a request or text-size limit.
func (s *identityJSON) shortString(span identitySpan, maximum int64) string {
	if span.end-span.start > maximum {
		return ""
	}
	raw := make([]byte, int(span.end-span.start))
	if _, err := s.file.ReadAt(raw, span.start); err != nil {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func (s *identityJSON) metadata(maximum int64) (string, error) {
	if err := s.whitespace(); err != nil {
		return "", err
	}
	b, err := s.peek()
	if err != nil {
		return "", err
	}
	if b != '"' {
		return "", s.skip()
	}
	span, err := s.string()
	if err != nil {
		return "", err
	}
	return s.shortString(span, maximum), nil
}

func (s *identityJSON) object(field func(string) error) error {
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > 10000 { // Match encoding/json's JSON nesting validation.
		return errIdentityJSON
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	if err := s.whitespace(); err != nil {
		return err
	}
	if b, _ := s.peek(); b == '}' {
		_, _ = s.byte()
		return nil
	}
	for {
		key, err := s.string()
		if err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		if err := s.whitespace(); err != nil {
			return err
		}
		if field == nil {
			err = s.skip()
		} else {
			err = field(s.shortString(key, 128))
		}
		if err != nil {
			return err
		}
		if err := s.whitespace(); err != nil {
			return err
		}
		b, err := s.byte()
		if err != nil {
			return err
		}
		if b == '}' {
			return nil
		}
		if b != ',' {
			return errIdentityJSON
		}
	}
}

func (s *identityJSON) array(value func() error) error {
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > 10000 {
		return errIdentityJSON
	}
	if err := s.expect('['); err != nil {
		return err
	}
	if err := s.whitespace(); err != nil {
		return err
	}
	if b, _ := s.peek(); b == ']' {
		_, _ = s.byte()
		return nil
	}
	for {
		if err := s.whitespace(); err != nil {
			return err
		}
		if err := value(); err != nil {
			return err
		}
		if err := s.whitespace(); err != nil {
			return err
		}
		b, err := s.byte()
		if err != nil {
			return err
		}
		if b == ']' {
			return nil
		}
		if b != ',' {
			return errIdentityJSON
		}
	}
}

func (s *identityJSON) digits() bool {
	found := false
	for {
		b, err := s.peek()
		if err != nil || b < '0' || b > '9' {
			return found
		}
		_, _ = s.byte()
		found = true
	}
}

func (s *identityJSON) number() error {
	if b, _ := s.peek(); b == '-' {
		_, _ = s.byte()
	}
	b, err := s.peek()
	if err != nil {
		return err
	}
	if b == '0' {
		_, _ = s.byte()
	} else if b >= '1' && b <= '9' {
		s.digits()
	} else {
		return errIdentityJSON
	}
	if b, _ := s.peek(); b == '.' {
		_, _ = s.byte()
		if !s.digits() {
			return errIdentityJSON
		}
	}
	if b, _ := s.peek(); b == 'e' || b == 'E' {
		_, _ = s.byte()
		if b, _ := s.peek(); b == '+' || b == '-' {
			_, _ = s.byte()
		}
		if !s.digits() {
			return errIdentityJSON
		}
	}
	return nil
}

func (s *identityJSON) skip() error {
	if err := s.whitespace(); err != nil {
		return err
	}
	b, err := s.peek()
	if err != nil {
		return err
	}
	switch b {
	case '{':
		return s.object(nil)
	case '[':
		return s.array(s.skip)
	case '"':
		_, err := s.string()
		return err
	case 't', 'f', 'n':
		word := "null"
		if b == 't' {
			word = "true"
		} else if b == 'f' {
			word = "false"
		}
		for i := range word {
			got, err := s.byte()
			if err != nil {
				return err
			}
			if got != word[i] {
				return errIdentityJSON
			}
		}
		return nil
	default:
		return s.number()
	}
}

func (s *identityJSON) appendCandidate(span identitySpan) error {
	if err := writeIdentitySpan(s.candidateWriter, span); err != nil {
		return err
	}
	s.candidateBytes += 16
	return nil
}

func (s *identityJSON) processCandidates(start int64) error {
	if err := s.candidateWriter.Flush(); err != nil {
		return err
	}
	rows := io.NewSectionReader(s.candidates, start, s.candidateBytes-start)
	for {
		span, err := readIdentitySpan(rows)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = scanIdentityText(identityTextSource{identityContextReaderAt{s.ctx, s.file}, identitySpan{span.start + 1, span.end - 1}, true}, func(removal identitySpan) error {
			if err := writeIdentitySpan(s.patchWriter, removal); err != nil {
				return err
			}
			s.patchCount++
			return nil
		})
		if err != nil {
			return err
		}
	}
}

func identityFieldSeen(seen *uint8, bit uint8) error {
	if *seen&bit != 0 {
		return errIdentityJSON // Ambiguous duplicate instruction fields stay untouched.
	}
	*seen |= bit
	return nil
}

func (s *identityJSON) content() error {
	b, err := s.peek()
	if err != nil {
		return err
	}
	if b == '"' {
		span, err := s.string()
		if err != nil {
			return err
		}
		return s.appendCandidate(span)
	}
	if b != '[' {
		return s.skip()
	}
	return s.array(func() error {
		if b, _ := s.peek(); b != '{' {
			return s.skip()
		}
		var kind string
		var span identitySpan
		var seen uint8
		err := s.object(func(key string) error {
			switch key {
			case "type":
				if err := identityFieldSeen(&seen, 1); err != nil {
					return err
				}
				var err error
				kind, err = s.metadata(128)
				return err
			case "text":
				if err := identityFieldSeen(&seen, 2); err != nil {
					return err
				}
				if b, _ := s.peek(); b != '"' {
					return s.skip()
				}
				var err error
				span, err = s.string()
				return err
			default:
				return s.skip()
			}
		})
		if err != nil {
			return err
		}
		if span.end > span.start && (seen&1 == 0 || kind == "text" || kind == "input_text") {
			return s.appendCandidate(span)
		}
		return nil
	})
}

func (s *identityJSON) messages() error {
	if b, _ := s.peek(); b != '[' {
		return s.skip()
	}
	return s.array(func() error {
		if b, _ := s.peek(); b != '{' {
			return s.skip()
		}
		start := s.candidateBytes
		var role, kind string
		var seen uint8
		err := s.object(func(key string) error {
			switch key {
			case "role", "type":
				bit := uint8(1)
				if key == "type" {
					bit = 2
				}
				if err := identityFieldSeen(&seen, bit); err != nil {
					return err
				}
				value, err := s.metadata(128)
				if key == "role" {
					role = value
				} else {
					kind = value
				}
				return err
			case "content":
				if err := identityFieldSeen(&seen, 4); err != nil {
					return err
				}
				return s.content()
			default:
				return s.skip()
			}
		})
		if err != nil {
			return err
		}
		if (role == "system" || role == "developer") && (seen&2 == 0 || kind == "message") {
			return s.processCandidates(start)
		}
		return nil
	})
}

func scanIdentityJSON(ctx context.Context, spools *identitySpools, file *os.File, size int64, path string) (*identityJSON, error) {
	candidates, err := spools.create()
	if err != nil {
		return nil, err
	}
	patches, err := spools.create()
	if err != nil {
		return nil, err
	}
	s := &identityJSON{ctx: ctx, file: file, r: bufio.NewReader(identityContextReader{ctx, io.NewSectionReader(file, 0, size)}), candidates: candidates, candidateWriter: bufio.NewWriter(candidates), patches: patches, patchWriter: bufio.NewWriter(patches)}
	var seen uint8
	err = s.object(func(key string) error {
		start := s.candidateBytes
		switch {
		case key == "model":
			if err := identityFieldSeen(&seen, 1); err != nil {
				return err
			}
			var err error
			s.model, err = s.metadata(2048)
			return err
		case key == "instructions" && strings.HasSuffix(path, "/responses"):
			if err := identityFieldSeen(&seen, 2); err != nil {
				return err
			}
			if b, _ := s.peek(); b != '"' {
				return s.skip()
			}
			span, err := s.string()
			if err != nil {
				return err
			}
			if err := s.appendCandidate(span); err != nil {
				return err
			}
		case (key == "input" && strings.HasSuffix(path, "/responses")) || (key == "messages" && strings.HasSuffix(path, "/chat/completions")):
			if err := identityFieldSeen(&seen, 4); err != nil {
				return err
			}
			return s.messages()
		case key == "system" && strings.HasSuffix(path, "/messages"):
			if err := identityFieldSeen(&seen, 2); err != nil {
				return err
			}
			if err := s.content(); err != nil {
				return err
			}
		case key == "systemInstruction" && strings.HasPrefix(path, "/v1beta/models/"):
			if err := identityFieldSeen(&seen, 2); err != nil {
				return err
			}
			if b, _ := s.peek(); b != '{' {
				return s.skip()
			}
			var partsSeen uint8
			if err := s.object(func(key string) error {
				if key != "parts" {
					return s.skip()
				}
				if err := identityFieldSeen(&partsSeen, 1); err != nil {
					return err
				}
				return s.content()
			}); err != nil {
				return err
			}
		default:
			return s.skip()
		}
		return s.processCandidates(start)
	})
	if err != nil {
		return nil, err
	}
	if err := s.whitespace(); err != nil {
		return nil, err
	}
	if _, err := s.peek(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errIdentityJSON
	}
	if err := s.patchWriter.Flush(); err != nil {
		return nil, err
	}
	if s.model == "" && strings.HasPrefix(path, "/v1beta/models/") {
		s.model, _, _ = strings.Cut(strings.TrimPrefix(path, "/v1beta/models/"), ":")
	}
	return s, nil
}

func applyIdentityPatches(ctx context.Context, spools *identitySpools, source *os.File, size int64, plan *identityJSON) (*os.File, int64, error) {
	output, err := spools.create()
	if err != nil {
		return nil, 0, err
	}
	w := bufio.NewWriter(output)
	patches := io.NewSectionReader(plan.patches, 0, plan.patchCount*16)
	var cursor, removed int64
	for i := int64(0); i < plan.patchCount; i++ {
		span, err := readIdentitySpan(patches)
		if err != nil {
			return nil, 0, err
		}
		if span.start < cursor || span.end < span.start || span.end > size {
			return nil, 0, errIdentityJSON
		}
		if _, err := identityCopy(ctx, w, io.NewSectionReader(source, cursor, span.start-cursor)); err != nil {
			return nil, 0, err
		}
		removed += span.end - span.start
		cursor = span.end
	}
	if _, err := identityCopy(ctx, w, io.NewSectionReader(source, cursor, size-cursor)); err != nil {
		return nil, 0, err
	}
	if err := w.Flush(); err != nil {
		return nil, 0, err
	}
	return output, size - removed, nil
}

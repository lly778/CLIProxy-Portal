package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Decode tool contracts, arguments, history and results without rounding IDs or
// amounts through float64. Typed integer fields still use encoding/json checks.
func unmarshalExact(data []byte, out any) error {
	if _, err := canonicalJSONNumbers(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid JSON trailing content")
	}
	return nil
}

// Keep hashes compatible with previously decoded ordinary floats, while exact
// integers remain distinct. Equivalent 1, 1.0 and 1e0 have one numeric identity.
// JSON strings (including serialized function arguments) are never rewritten.
func canonicalJSONNumbers(data []byte) ([]byte, error) {
	var out []byte
	quoted, escaped := false, false
	last := 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c != '-' && (c < '0' || c > '9') {
			continue
		}
		end := i + 1
		for end < len(data) && strings.ContainsRune("0123456789.eE+-", rune(data[end])) {
			end++
		}
		number, err := canonicalNumber(string(data[i:end]))
		if err != nil {
			return nil, err
		}
		if number != string(data[i:end]) {
			out = append(out, data[last:i]...)
			out = append(out, number...)
			last = end
		}
		i = end - 1
	}
	if out == nil {
		return data, nil
	}
	return append(out, data[last:]...), nil
}

func canonicalNumber(value string) (string, error) {
	invalid := errors.New("JSON number exceeds supported precision or exponent")
	if len(value) == 0 || len(value) > 256 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) || !json.Valid([]byte(value)) {
		return "", invalid
	}
	sign := ""
	if value[0] == '-' {
		sign, value = "-", value[1:]
	}
	exponent := 0
	if at := strings.IndexAny(value, "eE"); at >= 0 {
		var err error
		exponent, err = strconv.Atoi(value[at+1:])
		if err != nil || exponent < -1024 || exponent > 1024 {
			return "", invalid
		}
		value = value[:at]
	}
	if at := strings.IndexByte(value, '.'); at >= 0 {
		exponent -= len(value) - at - 1
		value = value[:at] + value[at+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(value, "0")
	exponent += len(value) - len(trimmed)
	value = trimmed
	point := len(value) + exponent
	if point > 21 || point <= -6 {
		mantissa := value[:1]
		if len(value) > 1 {
			mantissa += "." + value[1:]
		}
		e := strconv.Itoa(point - 1)
		if point > 0 {
			e = "+" + e
		}
		return sign + mantissa + "e" + e, nil
	}
	if point <= 0 {
		return sign + "0." + strings.Repeat("0", -point) + value, nil
	}
	if point < len(value) {
		return sign + value[:point] + "." + value[point:], nil
	}
	return sign + value + strings.Repeat("0", point-len(value)), nil
}

func numberRat(value any) (*big.Rat, bool) {
	var text string
	switch v := value.(type) {
	case json.Number:
		text = v.String()
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		text = strconv.FormatFloat(v, 'g', -1, 64)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	default:
		return nil, false
	}
	normalized, err := canonicalNumber(text)
	if err != nil {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(normalized)
	return r, ok
}

func numberInt(value any, minimum, maximum int64) (int64, bool) {
	r, ok := numberRat(value)
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	n := r.Num().Int64()
	return n, n >= minimum && n <= maximum
}

func jsonValueEqual(a, b any) bool {
	if x, ok := numberRat(a); ok {
		y, valid := numberRat(b)
		return valid && x.Cmp(y) == 0
	}
	// Stable JSON hashing handles nested numeric values and map order as well.
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false
	}
	x, err = canonicalJSONNumbers(x)
	if err != nil {
		return false
	}
	y, err = canonicalJSONNumbers(y)
	return err == nil && bytes.Equal(x, y)
}

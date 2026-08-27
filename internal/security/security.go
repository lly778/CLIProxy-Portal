package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

// Limit concurrent Argon2 work so the deliberately memory-hard password
// checks stay within the small container's memory budget under login bursts.
var argonSlots = make(chan struct{}, 1)

func HashPassword(password string) (string, error) {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	salt, err := RandomBytes(argonSaltLength)
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) bool {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func ValidatePassword(password, phone string, admin bool) error {
	min := 10
	if admin {
		min = 14
	}
	if utf8.RuneCountInString(password) < min {
		return fmt.Errorf("密码至少需要 %d 个字符", min)
	}
	if phone != "" && strings.Contains(password, phone) {
		return errors.New("密码不能包含完整手机号")
	}
	weak := map[string]bool{"1234567890": true, "password": true, "qwertyuiop": true, "adminadmin": true}
	if weak[strings.ToLower(strings.TrimSpace(password))] {
		return errors.New("密码过于常见")
	}
	return nil
}

func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func RandomToken(n int) (string, error) {
	b, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func NewID(prefix string) (string, error) {
	token, err := RandomToken(16)
	if err != nil {
		return "", err
	}
	return prefix + token, nil
}

func NewAPIKey() (string, error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", err
	}
	return "cpa_portal_" + token, nil
}

func NewResetCode() (string, error) {
	b, err := RandomBytes(10)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(base64.RawURLEncoding.EncodeToString(b)), nil
}

func SHA256(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func HMAC(secret []byte, value string) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

func CSRFToken(secret []byte, sessionToken string) string { return HMAC(secret, "csrf:"+sessionToken) }

func VerifyCSRF(secret []byte, sessionToken, token string) bool {
	want := CSRFToken(secret, sessionToken)
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

func NormalizeMainlandPhone(input string) (string, error) {
	replacer := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	phone := replacer.Replace(strings.TrimSpace(input))
	phone = strings.TrimPrefix(phone, "+86")
	phone = strings.TrimPrefix(phone, "0086")
	if len(phone) != 11 || phone[0] != '1' {
		return "", errors.New("请输入有效的中国大陆手机号")
	}
	for _, r := range phone {
		if r < '0' || r > '9' {
			return "", errors.New("请输入有效的中国大陆手机号")
		}
	}
	return phone, nil
}

func LastFour(value string) string {
	if len(value) <= 4 {
		return value
	}
	return value[len(value)-4:]
}

func ShortID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return strings.ToUpper(hex.EncodeToString(sum[:2]))
}

func Alias(name, phone, id string) string {
	return name + "-" + LastFour(phone) + "-" + ShortID(id)
}

func ParsePositiveInt(value string, fallback int) int {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

package waf

import (
	"strconv"
	"strings"
	"testing"
)

func TestSecurityCaptchaRequiresDigitsAndSingleUseChallenge(t *testing.T) {
	generator := NewCaptchaGenerator()
	id := generator.NewCaptcha(6)
	if len(id) != 16 {
		t.Fatal("captcha challenge identifier format changed")
	}
	// Non-digits must not be interpreted as zero-valued digits.
	generator.store.Set("fixture", []byte{0, 0, 0, 0})
	if generator.Verify("fixture", "aaaa") {
		t.Fatal("non-digit captcha text matched zero digits")
	}
	var parts []string
	for _, digit := range generator.Get(id) {
		parts = append(parts, strconv.Itoa(int(digit)))
	}
	answer := strings.Join(parts, "")
	if !generator.Verify(id, answer) || generator.Verify(id, answer) {
		t.Fatal("captcha challenge was not consumed exactly once")
	}
}

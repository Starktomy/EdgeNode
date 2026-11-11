package injectionutils_test

import (
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/waf/injectionutils"
	"github.com/TeaOSLab/EdgeNode/internal/waf/utils"
)

func TestSecuritySQLICacheSeparatesStrictMode(t *testing.T) {
	for _, strictFirst := range []bool{false, true} {
		padding := 140
		if strictFirst {
			padding++
		}
		input := "1 OR 1=1 -- " + strings.Repeat(" ", padding) + "\xff"
		if injectionutils.DetectSQLInjection(input, false) {
			t.Fatal("test requires a permissive UTF-8 rejection")
		}
		if !injectionutils.DetectSQLInjection(input, true) {
			t.Fatal("test requires a strict SQLI detection")
		}
		for _, strict := range []bool{strictFirst, !strictFirst, strictFirst, !strictFirst} {
			got := injectionutils.DetectSQLInjectionCache(input, strict, utils.CacheMiddleLife)
			if got != strict {
				t.Fatalf("strictFirst=%t, strict=%t, cached result=%t", strictFirst, strict, got)
			}
		}
	}
}

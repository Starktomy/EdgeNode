package injectionutils_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TeaOSLab/EdgeNode/internal/waf/injectionutils"
)

func TestSecurityNativeShortInputAndStrictModeCompatibility(t *testing.T) {
	for _, input := range []string{"", "a", "&", "&#", "&#x", "<span title=\"&#x\"></span>", "<div onani=x>", "<p \xff data-id=ok>"} {
		if injectionutils.DetectXSS(input, false) || injectionutils.DetectXSS(input, true) {
			t.Fatal("ordinary short HTML was rejected")
		}
	}
	if injectionutils.DetectXSS("<xml></xml>", false) || !injectionutils.DetectXSS("<xml></xml>", true) {
		t.Fatal("the GoEdge strict-mode distinction was lost")
	}
	for _, input := range []string{"", "1.e", "1.E", "12.5", "ordinary text"} {
		if injectionutils.DetectSQLInjection(input, true) {
			t.Fatal("ordinary numeric/text input was rejected")
		}
	}
}

func TestSecurityNativeParserBoundaries(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Fatal("the C compiler required by EdgeNode is unavailable")
	}
	output := filepath.Join(t.TempDir(), "native-boundaries")
	compile := exec.Command(compiler, "-O1", "-Ilibinjection/src", "testdata/native_boundaries.c", "testdata/native_sql_boundaries.c", "-o", output)
	if result, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile native boundary tests: %v\n%s", err, result)
	}
	if result, err := exec.Command(output).CombinedOutput(); err != nil {
		t.Fatalf("native boundary tests: %v\n%s", err, result)
	}
}

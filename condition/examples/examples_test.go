// Package examples runs every example program and compares its output with
// the expected_output.txt next to it, so the examples cannot silently drift
// from the library. Regenerate with: go test ./condition/examples -update
//
// examples 包运行每个示例程序，并将输出与同目录下的 expected_output.txt 比对，
// 确保示例不会在库变更后悄悄失效。重新生成：go test ./condition/examples -update
package examples

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite expected_output.txt files")

func TestExamples(t *testing.T) {
	dirs, err := filepath.Glob("*/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no examples found")
	}
	for _, mainFile := range dirs {
		dir := filepath.Dir(mainFile)
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command("go", "run", "./"+dir).CombinedOutput()
			if err != nil {
				t.Fatalf("go run ./%s: %v\n%s", dir, err, out)
			}
			golden := filepath.Join(dir, "expected_output.txt")
			if *update {
				if werr := os.WriteFile(golden, out, 0o644); werr != nil {
					t.Fatal(werr)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(want) {
				t.Fatalf("output mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", dir, out, want)
			}
		})
	}
}

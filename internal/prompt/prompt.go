package prompt

import (
	_ "embed"
	"os"
)

//go:embed system.txt
var System string

func init() {
	if b, err := os.ReadFile("prompt/system.txt"); err == nil && len(b) > 0 {
		System = string(b)
	}
}

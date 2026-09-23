package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(args map[string]any) (string, error)
}

func All() []Tool {
	return []Tool{readTool(), writeTool(), editTool(), bashTool(), globTool(), grepTool()}
}

func readTool() Tool {
	return Tool{
		Name:        "read",
		Description: "Read file or directory. filePath absolute.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}}, "required": []string{"filePath"}},
		Run: func(a map[string]any) (string, error) {
			p := a["filePath"].(string)
			fi, err := os.Stat(p)
			if err == nil && fi.IsDir() {
				ents, _ := os.ReadDir(p)
				var b strings.Builder
				for _, e := range ents {
					n := e.Name()
					if e.IsDir() {
						n += "/"
					}
					b.WriteString(n + "\n")
				}
				return b.String(), nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}
}

func writeTool() Tool {
	return Tool{
		Name:        "write",
		Description: "Write file. Creates or overwrites.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"filePath", "content"}},
		Run: func(a map[string]any) (string, error) {
			p := a["filePath"].(string)
			c := a["content"].(string)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			return "written", os.WriteFile(p, []byte(c), 0o644)
		},
	}
}

func editTool() Tool {
	return Tool{
		Name:        "edit",
		Description: "Exact string replacement. oldString must exist.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}, "oldString": map[string]any{"type": "string"}, "newString": map[string]any{"type": "string"}}, "required": []string{"filePath", "oldString", "newString"}},
		Run: func(a map[string]any) (string, error) {
			p := a["filePath"].(string)
			old := a["oldString"].(string)
			new := a["newString"].(string)
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			s := string(b)
			if !strings.Contains(s, old) {
				return "", fmt.Errorf("oldString not found")
			}
			s = strings.Replace(s, old, new, 1)
			return "edited", os.WriteFile(p, []byte(s), 0o644)
		},
	}
}

func bashTool() Tool {
	return Tool{
		Name:        "bash",
		Description: "Execute bash command.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
		Run: func(a map[string]any) (string, error) {
			c := a["command"].(string)
			out, err := exec.Command("bash", "-c", c).CombinedOutput()
			return string(out), err
		},
	}
}

func globTool() Tool {
	return Tool{
		Name:        "glob",
		Description: "Glob pattern search.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}, "required": []string{"pattern"}},
		Run: func(a map[string]any) (string, error) {
			m, _ := filepath.Glob(a["pattern"].(string))
			return strings.Join(m, "\n"), nil
		},
	}
}

func grepTool() Tool {
	return Tool{
		Name:        "grep",
		Description: "Grep pattern in files.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}, "required": []string{"pattern"}},
		Run: func(a map[string]any) (string, error) {
			p := a["pattern"].(string)
			out, _ := exec.Command("bash", "-c", "rg -n '"+strings.ReplaceAll(p, "'", "\\'")+"' 2>/dev/null | head -n 100").CombinedOutput()
			return string(out), nil
		},
	}
}

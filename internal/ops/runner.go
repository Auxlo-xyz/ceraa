package ops

import (
	"fmt"
	"soraa-agent/internal/store"
	"soraa-agent/internal/tools"
)

func Run(sess *store.Session, opID string) (string, error) {
	var op *store.Operation
	for i := range sess.Operations {
		if sess.Operations[i].ID == opID {
			op = &sess.Operations[i]
			break
		}
	}
	if op == nil {
		return "", fmt.Errorf("operation %s not found", opID)
	}

	op.Status = "running"
	sess.Save()

	tl := tools.All()
	var tool *tools.Tool
	for _, t := range tl {
		if t.Name == op.Tool {
			tool = &t
			break
		}
	}

	if tool == nil {
		op.Status = "failed"
		op.Error = "tool not found"
		sess.Save()
		return "", fmt.Errorf("tool %s not found", op.Tool)
	}

	out, err := tool.Run(op.Args)
	if err != nil {
		op.Status = "failed"
		op.Error = err.Error()
		sess.Save()
		return out, err
	}

	op.Status = "completed"
	op.Output = out
	sess.Save()
	return out, nil
}

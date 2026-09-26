package telegram

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// plainSendHelpers send text with no parse_mode, so a Telegram HTML tag in their
// arguments reaches the chat as literal text. That is what a user sees when
// <code> appears unrendered in a bubble.
var plainSendHelpers = map[string]bool{
	"send":                  true,
	"sendWithKeyboard":      true,
	"sendMessageWithMarkup": true,
	"sendPlainMessage":      true,
}

var htmlTags = []string{"<code>", "<b>", "<i>", "<pre>", "<a "}

// TestNoMarkupIsSentWithoutParseMode checks the call site, not a rendered
// message, because the defect belongs to the helper a message was passed to.
// Parsing the AST rather than grepping lines is what makes a multi-line
// concatenation like
//
//	sendHTML(token, chatID,
//		"...approve the code...\n\n"+flow.ResultMessage(),
//		markup)
//
// detectable at all; a line-based check only ever sees the tag on whichever line
// it happens to fall.
func TestNoMarkupIsSentWithoutParseMode(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || !plainSendHelpers[ident.Name] {
				return true
			}
			for _, arg := range call.Args {
				// The text is almost always a concatenation, so the tags live in
				// literals nested inside a BinaryExpr rather than in a direct
				// argument. Walk the whole argument expression.
				ast.Inspect(arg, func(inner ast.Node) bool {
					lit, ok := inner.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						return true
					}
					for _, tag := range htmlTags {
						if strings.Contains(value, tag) {
							position := fset.Position(call.Pos())
							offenders = append(offenders,
								position.Filename+":"+strconv.Itoa(position.Line)+" "+ident.Name+"() carries "+tag)
						}
					}
					return true
				})
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Errorf("markup passed to a send helper that sets no parse_mode, so it renders literally:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

package nuclei

// dsl.go implements the Nuclei DSL.
//
// The DSL is the small expression language the upstream corpus leans on: a
// template writes `status_code == 200 && contains(body, "admin")` and
// something has to decide it. It is implemented here as a language - a
// tokenizer, a recursive descent parser and an evaluator over the parsed
// tree - because an expression language evaluated by string matching is
// string matching wearing a language's coat, and the corpus is full of
// expressions that differ only in their punctuation.
//
// Three properties matter more than the breadth of the grammar:
//
//   - A template that cannot be evaluated must not look like a clean miss.
//     An unknown function, an unknown variable and a parse error are all
//     errors, and each names the expression and, where there is one, the
//     position.
//   - The work is bounded. The expression length, the node count and the
//     evaluation depth all have ceilings, so a hostile template cannot
//     hang a run.
//   - The evidence says why. A matcher that fires names the expression and
//     the value that decided it.
//
// The vocabulary is the one the corpus uses. The names that describe the
// response (status_code, body, all_headers, title and the rest) are
// resolved against the response; the names the template supplies (its own
// variables block, and whatever an extractor pulled out of an earlier
// response) are resolved against the context.

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The bounds. A hostile template must not be able to hang the run.
const (
	// maxDSLExpressionRunes bounds one expression.
	maxDSLExpressionRunes = 1000
	// maxDSLNodes bounds the size of the parsed tree.
	maxDSLNodes = 256
	// maxDSLDepth bounds the nesting of the parser and of the evaluator.
	maxDSLDepth = 32
	// maxDSLArgs bounds the arguments of one call.
	maxDSLArgs = 16
	// maxDSLWitnessRunes bounds the witness one node contributes.
	maxDSLWitnessRunes = 120
	// maxDSLArgRunes bounds one argument in a witness.
	maxDSLArgRunes = 40
	// maxCachedDSL bounds the parsed expression cache. A corpus of
	// thousands of templates runs the same expressions again and again.
	maxCachedDSL = 512
)

// ---------------------------------------------------------------------------
// The public surface
// ---------------------------------------------------------------------------

// DSLContext is what a DSL expression is evaluated against.
type DSLContext struct {
	// Response is the response the expression is looking at.
	Response Response
	// Vars are the names the template supplied: its own variables block and
	// the values the extractors pulled out of an earlier response.
	Vars map[string]string
}

// DSLResult is the outcome of one evaluation.
type DSLResult struct {
	// Value is the value the expression produced, as text.
	Value string
	// Bool is the boolean verdict. A matcher acts on this and on nothing
	// else, so a dsl matcher and a word matcher answer the same question.
	Bool bool
	// Number is the numeric value, when the expression produced one.
	Number float64
	// IsNumber says whether Number carries the value.
	IsNumber bool
	// Witness names the value that decided the expression.
	Witness string
}

// DSLExpr is a parsed Nuclei DSL expression.
//
// The tree is immutable once parsed, so one parsed expression is safe to
// share between the goroutines of a run.
type DSLExpr struct {
	source string
	root   dslNode
}

// Source returns the expression the tree was parsed from.
func (e *DSLExpr) Source() string {
	if e == nil {
		return ""
	}
	return e.source
}

// ParseDSL parses a Nuclei DSL expression into a tree.
//
// The error names the expression and the position of the problem: a
// template that does not parse is a template nobody can run, and the
// operator has to be able to see which part of it is wrong.
func ParseDSL(expr string) (*DSLExpr, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("the dsl expression is empty")
	}
	if length := len([]rune(trimmed)); length > maxDSLExpressionRunes {
		return nil, fmt.Errorf("the dsl expression is %d characters long, over the %d character ceiling", length, maxDSLExpressionRunes)
	}
	tokens, err := dslTokenize(trimmed)
	if err != nil {
		return nil, err
	}
	parser := &dslParser{source: trimmed, tokens: tokens}
	root, err := parser.parseExpression()
	if err != nil {
		return nil, err
	}
	if tok := parser.peek(); tok.kind != dslTokenEOF {
		return nil, dslSyntaxError(trimmed, tok.pos, "unexpected %s", describeDSLToken(tok))
	}
	return &DSLExpr{source: trimmed, root: root}, nil
}

// EvaluateDSL parses and evaluates one expression.
//
// The second result is an error whenever the expression could not be
// evaluated, including an unknown variable: a template that cannot be
// evaluated must not look like a clean miss.
func EvaluateDSL(expr string, ctx DSLContext) (DSLResult, error) {
	parsed, err := parseDSLCached(expr)
	if err != nil {
		return DSLResult{}, err
	}
	return parsed.Eval(ctx)
}

// Eval evaluates a parsed expression.
func (e *DSLExpr) Eval(ctx DSLContext) (DSLResult, error) {
	if e == nil || e.root == nil {
		return DSLResult{}, fmt.Errorf("the dsl expression is empty")
	}
	value, witness, err := e.root.eval(ctx, 0)
	if err != nil {
		return DSLResult{}, fmt.Errorf("the dsl expression %q could not be evaluated: %w", e.source, err)
	}
	return DSLResult{
		Value:    value.text,
		Bool:     dslTruthy(value),
		Number:   value.number,
		IsNumber: value.isNum,
		Witness:  witness,
	}, nil
}

// parseDSLCached parses an expression and keeps it.
//
// The cache exists for the same reason the regex cache does: a corpus of
// templates evaluates the same expressions over and over, and parsing one
// per response per template is a cost with no return.
func parseDSLCached(expr string) (*DSLExpr, error) {
	dslCacheMu.Lock()
	defer dslCacheMu.Unlock()
	if parsed, ok := dslCache[expr]; ok {
		return parsed, nil
	}
	parsed, err := ParseDSL(expr)
	if err != nil {
		return nil, err
	}
	if len(dslCache) >= maxCachedDSL {
		// The whole cache goes rather than one entry: the corpus is finite
		// and a full cache means a pathological one.
		dslCache = map[string]*DSLExpr{}
	}
	dslCache[expr] = parsed
	return parsed, nil
}

var (
	dslCacheMu sync.Mutex
	dslCache   = map[string]*DSLExpr{}
)

// ---------------------------------------------------------------------------
// The tokenizer
// ---------------------------------------------------------------------------

// dslTokenKind is the kind of one token.
type dslTokenKind int

const (
	dslTokenEOF dslTokenKind = iota
	dslTokenIdent
	dslTokenNumber
	dslTokenString
	dslTokenOp
	dslTokenLParen
	dslTokenRParen
	dslTokenComma
)

// dslToken is one token of an expression.
type dslToken struct {
	kind dslTokenKind
	text string
	// pos is the one based rune position of the token in the expression.
	pos int
}

// dslTokenize reads the tokens of an expression.
func dslTokenize(expr string) ([]dslToken, error) {
	runes := []rune(expr)
	var tokens []dslToken
	for i := 0; i < len(runes); {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			tokens = append(tokens, dslToken{kind: dslTokenLParen, text: "(", pos: i + 1})
			i++
		case c == ')':
			tokens = append(tokens, dslToken{kind: dslTokenRParen, text: ")", pos: i + 1})
			i++
		case c == ',':
			tokens = append(tokens, dslToken{kind: dslTokenComma, text: ",", pos: i + 1})
			i++
		case c == '"' || c == '\'':
			text, next, err := dslReadString(runes, i)
			if err != nil {
				return nil, dslSyntaxError(expr, i+1, "%s", err.Error())
			}
			tokens = append(tokens, dslToken{kind: dslTokenString, text: text, pos: i + 1})
			i = next
		case c >= '0' && c <= '9':
			end := i
			for end < len(runes) && (runes[end] >= '0' && runes[end] <= '9' || runes[end] == '.') {
				end++
			}
			tokens = append(tokens, dslToken{kind: dslTokenNumber, text: string(runes[i:end]), pos: i + 1})
			i = end
		case isDSLIdentStart(c):
			end := i
			for end < len(runes) && isDSLIdentPart(runes[end]) {
				end++
			}
			tokens = append(tokens, dslToken{kind: dslTokenIdent, text: string(runes[i:end]), pos: i + 1})
			i = end
		case c == '=':
			if i+1 < len(runes) && runes[i+1] == '=' {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: "==", pos: i + 1})
				i += 2
				continue
			}
			return nil, dslSyntaxError(expr, i+1, "a lone = is not an operator")
		case c == '!':
			if i+1 < len(runes) && runes[i+1] == '=' {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: "!=", pos: i + 1})
				i += 2
				continue
			}
			word, end := dslPeekWord(runes, i+1)
			// `!contains` is an operator, but `!contains(...)` is a negated
			// call. The difference is whether a parenthesis follows the
			// name, and only the tokenizer can see that far ahead.
			if (word == "contains" || word == "matches") && !(end < len(runes) && runes[end] == '(') {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: "!" + word, pos: i + 1})
				i = end
				continue
			}
			tokens = append(tokens, dslToken{kind: dslTokenOp, text: "!", pos: i + 1})
			i++
		case c == '>' || c == '<':
			if i+1 < len(runes) && runes[i+1] == '=' {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: string(c) + "=", pos: i + 1})
				i += 2
				continue
			}
			tokens = append(tokens, dslToken{kind: dslTokenOp, text: string(c), pos: i + 1})
			i++
		case c == '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: "&&", pos: i + 1})
				i += 2
				continue
			}
			return nil, dslSyntaxError(expr, i+1, "a lone & is not an operator")
		case c == '|':
			if i+1 < len(runes) && runes[i+1] == '|' {
				tokens = append(tokens, dslToken{kind: dslTokenOp, text: "||", pos: i + 1})
				i += 2
				continue
			}
			return nil, dslSyntaxError(expr, i+1, "a lone | is not an operator")
		default:
			return nil, dslSyntaxError(expr, i+1, "the character %q is not part of an expression", string(c))
		}
	}
	tokens = append(tokens, dslToken{kind: dslTokenEOF, pos: len(runes) + 1})
	return tokens, nil
}

// dslReadString reads a quoted literal.
func dslReadString(runes []rune, start int) (string, int, error) {
	quote := runes[start]
	var out strings.Builder
	for i := start + 1; i < len(runes); i++ {
		c := runes[i]
		if c == '\\' && i+1 < len(runes) {
			i++
			switch runes[i] {
			case 'n':
				out.WriteRune('\n')
			case 'r':
				out.WriteRune('\r')
			case 't':
				out.WriteRune('\t')
			case '\\':
				out.WriteRune('\\')
			case '"':
				out.WriteRune('"')
			case '\'':
				out.WriteRune('\'')
			default:
				out.WriteRune('\\')
				out.WriteRune(runes[i])
			}
			continue
		}
		if c == quote {
			return out.String(), i + 1, nil
		}
		out.WriteRune(c)
	}
	return "", start, fmt.Errorf("the quoted literal is not closed")
}

// dslPeekWord reads the identifier that starts at the given position.
func dslPeekWord(runes []rune, start int) (string, int) {
	if start >= len(runes) || !isDSLIdentStart(runes[start]) {
		return "", start
	}
	end := start
	for end < len(runes) && isDSLIdentPart(runes[end]) {
		end++
	}
	return string(runes[start:end]), end
}

// isDSLIdentStart reports whether a rune can start an identifier.
func isDSLIdentStart(c rune) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isDSLIdentPart reports whether a rune can continue an identifier.
func isDSLIdentPart(c rune) bool {
	return isDSLIdentStart(c) || c >= '0' && c <= '9'
}

// describeDSLToken names a token in an error.
func describeDSLToken(tok dslToken) string {
	if tok.kind == dslTokenEOF {
		return "the end of the expression"
	}
	return strconv.Quote(tok.text)
}

// dslSyntaxError names the expression and the position of the problem.
func dslSyntaxError(expr string, pos int, format string, args ...any) error {
	return fmt.Errorf("the dsl expression %q does not parse: %s at position %d", expr, fmt.Sprintf(format, args...), pos)
}

// ---------------------------------------------------------------------------
// The parser
// ---------------------------------------------------------------------------

// dslParser is a recursive descent parser over the tokens.
type dslParser struct {
	source string
	tokens []dslToken
	pos    int
	nodes  int
	depth  int
}

// parseExpression parses one expression.
func (p *dslParser) parseExpression() (dslNode, error) {
	return p.parseOr()
}

// parseOr parses the || level.
func (p *dslParser) parseOr() (dslNode, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == dslTokenOp && p.peek().text == "||" {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left, err = p.node(&dslBinaryNode{op: "||", left: left, right: right})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

// parseAnd parses the && level.
func (p *dslParser) parseAnd() (dslNode, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == dslTokenOp && p.peek().text == "&&" {
		p.next()
		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		left, err = p.node(&dslBinaryNode{op: "&&", left: left, right: right})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

// parseComparison parses the comparison level.
//
// The comparison operators are the ones the corpus uses: ==, !=, contains,
// !contains, matches, !matches and the four comparisons. `contains` and
// `matches` are words, so the parser has to tell the operator from the
// function of the same name: a parenthesis after the name means a call.
func (p *dslParser) parseComparison() (dslNode, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.comparisonOperator()
		if !ok {
			return left, nil
		}
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left, err = p.node(&dslBinaryNode{op: op, left: left, right: right})
		if err != nil {
			return nil, err
		}
	}
}

// comparisonOperator reports the comparison operator at the cursor.
func (p *dslParser) comparisonOperator() (string, bool) {
	tok := p.peek()
	switch tok.kind {
	case dslTokenOp:
		switch tok.text {
		case "==", "!=", "contains", "!contains", "matches", "!matches", ">", "<", ">=", "<=":
			return tok.text, true
		}
		return "", false
	case dslTokenIdent:
		if tok.text != "contains" && tok.text != "matches" {
			return "", false
		}
		if p.peekAt(1).kind == dslTokenLParen {
			return "", false
		}
		return tok.text, true
	}
	return "", false
}

// parseUnary parses the ! level.
func (p *dslParser) parseUnary() (dslNode, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	if tok := p.peek(); tok.kind == dslTokenOp && tok.text == "!" {
		p.next()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return p.node(&dslUnaryNode{op: "!", operand: operand})
	}
	return p.parsePrimary()
}

// parsePrimary parses a value.
func (p *dslParser) parsePrimary() (dslNode, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	tok := p.peek()
	switch tok.kind {
	case dslTokenNumber:
		p.next()
		value, err := strconv.ParseFloat(tok.text, 64)
		if err != nil {
			return nil, dslSyntaxError(p.source, tok.pos, "%q is not a number", tok.text)
		}
		return p.node(&dslLiteralNode{value: dslNumber(value)})
	case dslTokenString:
		p.next()
		return p.node(&dslLiteralNode{value: dslString(tok.text)})
	case dslTokenIdent:
		switch tok.text {
		case "true":
			p.next()
			return p.node(&dslLiteralNode{value: dslBool(true)})
		case "false":
			p.next()
			return p.node(&dslLiteralNode{value: dslBool(false)})
		}
		if p.peekAt(1).kind == dslTokenLParen {
			return p.parseCall()
		}
		p.next()
		return p.node(&dslVarNode{name: tok.text})
	case dslTokenLParen:
		p.next()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		closing := p.peek()
		if closing.kind != dslTokenRParen {
			return nil, dslSyntaxError(p.source, closing.pos, "expected a closing parenthesis, found %s", describeDSLToken(closing))
		}
		p.next()
		return inner, nil
	}
	return nil, dslSyntaxError(p.source, tok.pos, "expected a value, found %s", describeDSLToken(tok))
}

// parseCall parses a function call.
func (p *dslParser) parseCall() (dslNode, error) {
	name := p.next()
	arity, known := dslFunctions[name.text]
	if !known {
		return nil, dslSyntaxError(p.source, name.pos, "the function %q is not one this engine knows", name.text)
	}
	p.next() // the opening parenthesis
	var args []dslNode
	for {
		if p.peek().kind == dslTokenRParen {
			break
		}
		arg, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if len(args) > maxDSLArgs {
			return nil, dslSyntaxError(p.source, name.pos, "the function %q takes at most %d arguments", name.text, maxDSLArgs)
		}
		if p.peek().kind == dslTokenComma {
			p.next()
			continue
		}
		break
	}
	closing := p.peek()
	if closing.kind != dslTokenRParen {
		return nil, dslSyntaxError(p.source, closing.pos, "expected a closing parenthesis, found %s", describeDSLToken(closing))
	}
	p.next()
	if len(args) < arity.min || arity.max >= 0 && len(args) > arity.max {
		return nil, dslSyntaxError(p.source, name.pos, "the function %q takes %s arguments, %d were given", name.text, dslArityText(arity), len(args))
	}
	return p.node(&dslCallNode{name: name.text, args: args})
}

// node records a node and holds the tree to its ceiling.
func (p *dslParser) node(n dslNode) (dslNode, error) {
	p.nodes++
	if p.nodes > maxDSLNodes {
		return nil, dslSyntaxError(p.source, p.peek().pos, "the expression has more than %d nodes", maxDSLNodes)
	}
	return n, nil
}

// enter holds the nesting of the parser to its ceiling.
func (p *dslParser) enter() error {
	p.depth++
	if p.depth > maxDSLDepth {
		return dslSyntaxError(p.source, p.peek().pos, "the expression nests deeper than %d levels", maxDSLDepth)
	}
	return nil
}

// leave leaves one level of the parser.
func (p *dslParser) leave() {
	p.depth--
}

// peek returns the token at the cursor.
func (p *dslParser) peek() dslToken {
	return p.peekAt(0)
}

// peekAt returns the token n places after the cursor.
func (p *dslParser) peekAt(n int) dslToken {
	index := p.pos + n
	if index >= len(p.tokens) {
		return dslToken{kind: dslTokenEOF, pos: len(p.tokens)}
	}
	return p.tokens[index]
}

// next consumes the token at the cursor.
func (p *dslParser) next() dslToken {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

// ---------------------------------------------------------------------------
// The tree
// ---------------------------------------------------------------------------

// dslNode is one node of a parsed expression.
type dslNode interface {
	// eval evaluates the node and returns its value and the witness that
	// decided it.
	eval(ctx DSLContext, depth int) (dslValue, string, error)
}

// dslLiteralNode is a constant.
type dslLiteralNode struct {
	value dslValue
}

// dslVarNode is a name.
type dslVarNode struct {
	name string
}

// dslCallNode is a function call.
type dslCallNode struct {
	name string
	args []dslNode
}

// dslUnaryNode is a one operand operator.
type dslUnaryNode struct {
	op      string
	operand dslNode
}

// dslBinaryNode is a two operand operator.
type dslBinaryNode struct {
	op          string
	left, right dslNode
}

// ---------------------------------------------------------------------------
// The values
// ---------------------------------------------------------------------------

// dslValue is one value of the language.
//
// The language has three kinds of value and a number keeps its numeric
// self, because `status_code == 200` has to compare 200 with 200 and not
// with two strings.
type dslValue struct {
	text    string
	number  float64
	isNum   bool
	isBool  bool
	boolean bool
}

// dslString makes a string value.
func dslString(s string) dslValue {
	return dslValue{text: s}
}

// dslNumber makes a numeric value.
func dslNumber(n float64) dslValue {
	return dslValue{text: strconv.FormatFloat(n, 'f', -1, 64), number: n, isNum: true}
}

// dslBool makes a boolean value.
func dslBool(b bool) dslValue {
	return dslValue{text: strconv.FormatBool(b), isBool: true, boolean: b}
}

// dslTruthy reports whether a value counts as true.
func dslTruthy(v dslValue) bool {
	switch {
	case v.isBool:
		return v.boolean
	case v.isNum:
		return v.number != 0
	default:
		trimmed := strings.TrimSpace(v.text)
		if trimmed == "" {
			return false
		}
		if strings.EqualFold(trimmed, "false") {
			return false
		}
		return trimmed != "0"
	}
}

// dslNumberOf reads a value as a number, whether it is one or its text
// happens to be one.
func dslNumberOf(v dslValue) (float64, bool) {
	if v.isNum {
		return v.number, true
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(v.text), 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// ---------------------------------------------------------------------------
// The evaluator
// ---------------------------------------------------------------------------

// eval evaluates a literal.
func (n *dslLiteralNode) eval(DSLContext, int) (dslValue, string, error) {
	return n.value, n.value.text, nil
}

// eval evaluates a name.
func (n *dslVarNode) eval(ctx DSLContext, _ int) (dslValue, string, error) {
	if value, witness, ok := dslResponseVariable(n.name, ctx); ok {
		return value, witness, nil
	}
	if value, ok := ctx.Vars[n.name]; ok {
		return dslString(value), n.name + " = " + clipRunes(value, maxDSLArgRunes), nil
	}
	if arity, known := dslFunctions[n.name]; known {
		return dslValue{}, "", fmt.Errorf("the name %q is a function; call %s(%s)", n.name, n.name, dslArityText(arity))
	}
	return dslValue{}, "", fmt.Errorf("the variable %q is not one this engine knows and the template did not supply it", n.name)
}

// eval evaluates a call.
func (n *dslCallNode) eval(ctx DSLContext, depth int) (dslValue, string, error) {
	if depth > maxDSLDepth {
		return dslValue{}, "", fmt.Errorf("the expression nests deeper than %d levels", maxDSLDepth)
	}
	args := make([]dslValue, 0, len(n.args))
	for _, arg := range n.args {
		value, _, err := arg.eval(ctx, depth+1)
		if err != nil {
			return dslValue{}, "", err
		}
		args = append(args, value)
	}
	result, err := dslCall(n.name, args, ctx)
	if err != nil {
		return dslValue{}, "", err
	}
	return result, dslCallWitness(n.name, args, result), nil
}

// eval evaluates a one operand operator.
func (n *dslUnaryNode) eval(ctx DSLContext, depth int) (dslValue, string, error) {
	if depth > maxDSLDepth {
		return dslValue{}, "", fmt.Errorf("the expression nests deeper than %d levels", maxDSLDepth)
	}
	operand, witness, err := n.operand.eval(ctx, depth+1)
	if err != nil {
		return dslValue{}, "", err
	}
	switch n.op {
	case "!":
		return dslBool(!dslTruthy(operand)), "not " + witness, nil
	}
	return dslValue{}, "", fmt.Errorf("the operator %q is not one this engine knows", n.op)
}

// eval evaluates a two operand operator.
func (n *dslBinaryNode) eval(ctx DSLContext, depth int) (dslValue, string, error) {
	if depth > maxDSLDepth {
		return dslValue{}, "", fmt.Errorf("the expression nests deeper than %d levels", maxDSLDepth)
	}
	if n.op == "&&" || n.op == "||" {
		left, leftWitness, err := n.left.eval(ctx, depth+1)
		if err != nil {
			return dslValue{}, "", err
		}
		if n.op == "&&" && !dslTruthy(left) {
			return dslBool(false), leftWitness, nil
		}
		if n.op == "||" && dslTruthy(left) {
			return dslBool(true), leftWitness, nil
		}
		right, rightWitness, err := n.right.eval(ctx, depth+1)
		if err != nil {
			return dslValue{}, "", err
		}
		return dslBool(dslTruthy(right)), rightWitness, nil
	}
	left, leftWitness, err := n.left.eval(ctx, depth+1)
	if err != nil {
		return dslValue{}, "", err
	}
	right, rightWitness, err := n.right.eval(ctx, depth+1)
	if err != nil {
		return dslValue{}, "", err
	}
	result, err := dslCompare(n.op, left, right)
	if err != nil {
		return dslValue{}, "", err
	}
	witness := leftWitness + " " + n.op + " " + rightWitness
	return result, clipRunes(witness, maxDSLWitnessRunes), nil
}

// dslCompare applies a comparison operator.
func dslCompare(op string, left, right dslValue) (dslValue, error) {
	switch op {
	case "==":
		if left.isNum && right.isNum {
			return dslBool(left.number == right.number), nil
		}
		return dslBool(left.text == right.text), nil
	case "!=":
		if left.isNum && right.isNum {
			return dslBool(left.number != right.number), nil
		}
		return dslBool(left.text != right.text), nil
	case "contains":
		return dslBool(strings.Contains(left.text, right.text)), nil
	case "!contains":
		return dslBool(!strings.Contains(left.text, right.text)), nil
	case "matches", "!matches":
		compiled, err := compileRegex(right.text, false)
		if err != nil {
			return dslValue{}, fmt.Errorf("the pattern %q does not compile: %w", right.text, err)
		}
		matched := compiled.MatchString(left.text)
		if op == "!matches" {
			matched = !matched
		}
		return dslBool(matched), nil
	case ">", "<", ">=", "<=":
		leftNumber, leftOk := dslNumberOf(left)
		rightNumber, rightOk := dslNumberOf(right)
		if !leftOk || !rightOk {
			return dslValue{}, fmt.Errorf("%s cannot compare %q with %q: one of them is not a number", op, clipRunes(left.text, maxDSLArgRunes), clipRunes(right.text, maxDSLArgRunes))
		}
		switch op {
		case ">":
			return dslBool(leftNumber > rightNumber), nil
		case "<":
			return dslBool(leftNumber < rightNumber), nil
		case ">=":
			return dslBool(leftNumber >= rightNumber), nil
		default:
			return dslBool(leftNumber <= rightNumber), nil
		}
	}
	return dslValue{}, fmt.Errorf("the operator %q is not one this engine knows", op)
}

// ---------------------------------------------------------------------------
// The functions
// ---------------------------------------------------------------------------

// dslFuncArity is how many arguments a function takes.
type dslFuncArity struct {
	min int
	// max is the largest count, or -1 when there is no upper bound.
	max int
}

// dslFunctions is the function table.
//
// The table is also the arity check and the "is this function known" check,
// so an unknown function is a parse error rather than a runtime surprise.
var dslFunctions = map[string]dslFuncArity{
	"contains":      {min: 2, max: 2},
	"regex":         {min: 2, max: 2},
	"len":           {min: 1, max: 1},
	"md5":           {min: 1, max: 1},
	"sha1":          {min: 1, max: 1},
	"sha256":        {min: 1, max: 1},
	"base64":        {min: 1, max: 1},
	"base64_decode": {min: 1, max: 1},
	"to_lower":      {min: 1, max: 1},
	"to_upper":      {min: 1, max: 1},
	"lower":         {min: 1, max: 1},
	"upper":         {min: 1, max: 1},
	"concat":        {min: 2, max: maxDSLArgs},
	"starts_with":   {min: 2, max: 2},
	"ends_with":     {min: 2, max: 2},
	"header_name":   {min: 1, max: 1},
	"cookie":        {min: 1, max: 1},
	"rand_base64":   {min: 0, max: 1},
}

// dslArityText names an arity in an error.
func dslArityText(arity dslFuncArity) string {
	if arity.max < 0 {
		return fmt.Sprintf("at least %d", arity.min)
	}
	if arity.min == arity.max {
		return strconv.Itoa(arity.min)
	}
	return fmt.Sprintf("%d to %d", arity.min, arity.max)
}

// dslCall applies a function.
func dslCall(name string, args []dslValue, ctx DSLContext) (dslValue, error) {
	switch name {
	case "contains":
		return dslBool(strings.Contains(args[0].text, args[1].text)), nil
	case "regex":
		compiled, err := compileRegex(args[1].text, false)
		if err != nil {
			return dslValue{}, fmt.Errorf("the pattern %q does not compile: %w", args[1].text, err)
		}
		return dslBool(compiled.MatchString(args[0].text)), nil
	case "len":
		return dslNumber(float64(len([]rune(args[0].text)))), nil
	case "md5":
		sum := md5.Sum([]byte(args[0].text))
		return dslString(hex.EncodeToString(sum[:])), nil
	case "sha1":
		sum := sha1.Sum([]byte(args[0].text))
		return dslString(hex.EncodeToString(sum[:])), nil
	case "sha256":
		sum := sha256.Sum256([]byte(args[0].text))
		return dslString(hex.EncodeToString(sum[:])), nil
	case "base64":
		return dslString(base64.StdEncoding.EncodeToString([]byte(args[0].text))), nil
	case "base64_decode":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(args[0].text))
		if err != nil {
			raw, rawErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(args[0].text))
			if rawErr != nil {
				return dslValue{}, fmt.Errorf("the value is not base64: %w", err)
			}
			decoded = raw
		}
		return dslString(string(decoded)), nil
	case "to_lower", "lower":
		return dslString(strings.ToLower(args[0].text)), nil
	case "to_upper", "upper":
		return dslString(strings.ToUpper(args[0].text)), nil
	case "concat":
		var b strings.Builder
		for _, arg := range args {
			b.WriteString(arg.text)
		}
		return dslString(b.String()), nil
	case "starts_with":
		return dslBool(strings.HasPrefix(args[0].text, args[1].text)), nil
	case "ends_with":
		return dslBool(strings.HasSuffix(args[0].text, args[1].text)), nil
	case "header_name":
		return dslString(headerValue(ctx.Response.Headers, args[0].text)), nil
	case "cookie":
		return dslString(cookieValue(ctx.Response.Headers, args[0].text)), nil
	case "rand_base64":
		length := 12
		if len(args) == 1 {
			if n, ok := dslNumberOf(args[0]); ok && n > 0 && n <= 64 {
				length = int(n)
			}
		}
		return dslString(base64.StdEncoding.EncodeToString([]byte(randomString(length)))), nil
	}
	return dslValue{}, fmt.Errorf("the function %q is not one this engine knows", name)
}

// dslCallWitness describes what a call returned.
func dslCallWitness(name string, args []dslValue, result dslValue) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, clipRunes(arg.text, maxDSLArgRunes))
	}
	return clipRunes(name+"("+strings.Join(parts, ", ")+") = "+result.text, maxDSLWitnessRunes)
}

// ---------------------------------------------------------------------------
// The variables
// ---------------------------------------------------------------------------

// dslResponseVariable resolves a name against the response.
//
// The language's own names come first: a template that defines a variable
// called "body" has not redefined what the body is.
func dslResponseVariable(name string, ctx DSLContext) (dslValue, string, bool) {
	response := ctx.Response
	switch name {
	case "status_code":
		value := strconv.Itoa(response.Status)
		return dslNumber(float64(response.Status)), "status_code = " + value, true
	case "content_length":
		value := strconv.Itoa(len(response.Body))
		return dslNumber(float64(len(response.Body))), "content_length = " + value, true
	case "body":
		body := string(response.Body)
		return dslString(body), "body = " + clipRunes(body, maxDSLArgRunes), true
	case "all_headers":
		headers := headerText(response.Headers)
		return dslString(headers), "all_headers = " + clipRunes(headers, maxDSLArgRunes), true
	case "response":
		text := responseText(response)
		return dslString(text), "response = " + clipRunes(text, maxDSLArgRunes), true
	case "title":
		title := responseTitle(response.Body)
		return dslString(title), "title = " + clipRunes(title, maxDSLArgRunes), true
	case "content_type":
		value := headerValue(response.Headers, "content-type")
		return dslString(value), "content_type = " + clipRunes(value, maxDSLArgRunes), true
	case "method":
		method := response.Method
		if method == "" {
			method = "GET"
		}
		return dslString(method), "method = " + method, true
	case "host":
		host := responseHost(response.URL)
		return dslString(host), "host = " + host, true
	case "path":
		path := responsePath(response.URL)
		return dslString(path), "path = " + path, true
	case "query":
		query := responseQuery(response.URL)
		return dslString(query), "query = " + query, true
	case "request":
		head := responseRequestHead(response)
		return dslString(head), "request = " + clipRunes(head, maxDSLArgRunes), true
	case "raw":
		raw := responseRequestHead(response) + response.RequestBody
		return dslString(raw), "raw = " + clipRunes(raw, maxDSLArgRunes), true
	case "uuid":
		value := randomUUID()
		return dslString(value), "uuid = " + value, true
	case "rand_base64":
		value := base64.StdEncoding.EncodeToString([]byte(randomString(12)))
		return dslString(value), "rand_base64 = " + clipRunes(value, maxDSLArgRunes), true
	}
	return dslValue{}, "", false
}

// titleRegex reads the title out of a body.
var titleRegex = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// responseTitle reads the title of a body.
func responseTitle(body []byte) string {
	match := titleRegex.FindSubmatch(body)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}

// headerValue reads a header by name, ignoring the case of the name.
func headerValue(headers map[string]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if value, ok := headers[name]; ok {
		return value
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

// cookieValue reads a cookie out of the set-cookie header.
func cookieValue(headers map[string]string, name string) string {
	raw := headerValue(headers, "set-cookie")
	if raw == "" {
		return ""
	}
	for _, part := range strings.Split(raw, ";") {
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(key) == name {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// responseHost reads the host out of a URL.
func responseHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// responsePath reads the path out of a URL.
func responsePath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path := parsed.Path
	if path == "" {
		return "/"
	}
	return path
}

// responseQuery reads the query out of a URL.
func responseQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.RawQuery
}

// responseRequestHead renders the request line and the headers of the
// request that produced the response.
func responseRequestHead(response Response) string {
	method := response.Method
	if method == "" {
		method = "GET"
	}
	target := response.URL
	if parsed, err := url.Parse(response.URL); err == nil {
		target = parsed.RequestURI()
		if target == "" {
			target = "/"
		}
	}
	var b strings.Builder
	b.WriteString(method)
	b.WriteString(" ")
	b.WriteString(target)
	b.WriteString(" HTTP/1.1")
	b.WriteString("\n")
	names := make([]string, 0, len(response.RequestHeaders))
	for name := range response.RequestHeaders {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(response.RequestHeaders[name])
		b.WriteString("\n")
	}
	return b.String()
}

// responseText renders the whole response: the status line, the headers
// and the body.
func responseText(response Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d\n", response.Status)
	names := make([]string, 0, len(response.Headers))
	for name := range response.Headers {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(response.Headers[name])
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.Write(response.Body)
	return b.String()
}

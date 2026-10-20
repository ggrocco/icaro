package expr

import (
	"fmt"
	"strconv"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tOp
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

func (t token) String() string {
	switch t.kind {
	case tEOF:
		return "end of expression"
	case tString:
		return strconv.Quote(t.text)
	default:
		return fmt.Sprintf("%q", t.text)
	}
}

type parser struct {
	src  string
	toks []token
	i    int
}

func (p *parser) errAt(pos int, format string, args ...any) error {
	return &Error{Src: p.src, Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || c == '-' || (c >= '0' && c <= '9')
}

func (p *parser) lex() error {
	s := p.src
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && isIdentChar(s[j]) {
				j++
			}
			// A trailing '-' belongs to nothing useful; keep idents clean.
			for j > i+1 && s[j-1] == '-' {
				j--
			}
			p.toks = append(p.toks, token{kind: tIdent, text: s[i:j], pos: i})
			i = j
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' && p.prevAllowsUnary()):
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			f, err := strconv.ParseFloat(s[i:j], 64)
			if err != nil {
				return p.errAt(i, "invalid number %q", s[i:j])
			}
			p.toks = append(p.toks, token{kind: tNumber, text: s[i:j], num: f, pos: i})
			i = j
		case c == '\'' || c == '"':
			var sb strings.Builder
			j := i + 1
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					switch s[j+1] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(s[j+1])
					}
					j += 2
					continue
				}
				if s[j] == c {
					closed = true
					j++
					break
				}
				sb.WriteByte(s[j])
				j++
			}
			if !closed {
				return p.errAt(i, "unterminated string")
			}
			p.toks = append(p.toks, token{kind: tString, text: sb.String(), pos: i})
			i = j
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				p.toks = append(p.toks, token{kind: tOp, text: two, pos: i})
				i += 2
				continue
			}
			switch c {
			case '<', '>', '!', '.', '[', ']', '(', ')', ',':
				p.toks = append(p.toks, token{kind: tOp, text: string(c), pos: i})
				i++
			case '=':
				return p.errAt(i, "use == for comparison")
			default:
				return p.errAt(i, "unexpected character %q", c)
			}
		}
	}
	p.toks = append(p.toks, token{kind: tEOF, pos: len(s)})
	return nil
}

// prevAllowsUnary reports whether a '-' here starts a negative number.
func (p *parser) prevAllowsUnary() bool {
	if len(p.toks) == 0 {
		return true
	}
	t := p.toks[len(p.toks)-1]
	return t.kind == tOp && t.text != ")" && t.text != "]"
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) accept(op string) bool {
	if t := p.peek(); t.kind == tOp && t.text == op {
		p.i++
		return true
	}
	return false
}

func (p *parser) expect(op string) error {
	if !p.accept(op) {
		return p.errAt(p.peek().pos, "expected %q, found %s", op, p.peek())
	}
	return nil
}

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept("||") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &logicNode{op: "||", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.accept("&&") {
		r, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		l = &logicNode{op: "&&", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseCmp() (node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tOp {
		switch t.text {
		case "==", "!=", "<", "<=", ">", ">=":
			p.i++
			r, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			return &cmpNode{op: t.text, l: l, r: r, pos: t.pos}, nil
		}
	}
	return l, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.accept("!") {
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &notNode{n: n}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("."):
			t := p.next()
			if t.kind != tIdent {
				return nil, p.errAt(t.pos, "expected a name after '.', found %s", t)
			}
			n = &indexNode{base: n, key: &litNode{v: t.text}, static: t.text}
		case p.accept("["):
			k, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			in := &indexNode{base: n, key: k}
			if lit, ok := k.(*litNode); ok {
				switch v := lit.v.(type) {
				case string:
					in.static = v
				case float64:
					in.static = strconv.Itoa(int(v))
				}
			}
			n = in
		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.next()
	switch t.kind {
	case tNumber:
		return &litNode{v: t.num}, nil
	case tString:
		return &litNode{v: t.text}, nil
	case tIdent:
		switch t.text {
		case "true":
			return &litNode{v: true}, nil
		case "false":
			return &litNode{v: false}, nil
		case "null":
			return &litNode{v: nil}, nil
		}
		if p.accept("(") {
			fn, ok := funcs[t.text]
			if !ok && t.text != "has" {
				msg := fmt.Sprintf("unknown function %q", t.text)
				if h := Nearest(t.text, FuncNames()); h != "" {
					msg += fmt.Sprintf("; did you mean %q?", h)
				}
				return nil, p.errAt(t.pos, "%s", msg)
			}
			var args []node
			if !p.accept(")") {
				for {
					a, err := p.parseOr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.accept(")") {
						break
					}
					if err := p.expect(","); err != nil {
						return nil, err
					}
				}
			}
			if t.text == "has" {
				if len(args) != 1 {
					return nil, p.errAt(t.pos, "has() takes one reference")
				}
				if _, ok := args[0].(*indexNode); !ok {
					return nil, p.errAt(t.pos, "has() takes a reference such as has(trigger.label)")
				}
				return &hasNode{ref: args[0]}, nil
			}
			if len(args) < fn.min || (fn.max >= 0 && len(args) > fn.max) {
				return nil, p.errAt(t.pos, "%s() takes %s argument(s), got %d", t.text, fn.arity(), len(args))
			}
			return &callNode{name: t.text, fn: fn, args: args}, nil
		}
		return &rootNode{name: t.text}, nil
	case tOp:
		if t.text == "(" {
			n, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, p.errAt(t.pos, "unexpected %s", t)
}

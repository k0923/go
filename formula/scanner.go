package formula

import (
	"unicode"
	"unicode/utf8"
	"go/token"
)

const eof = -1

func NewFormulaScanner(expression string) *Scanner {
	src := []rune(expression)
	sc := &Scanner{
		src:    src,
		offset: -1,
	}
	sc.next()
	return sc
}

type Scanner struct {
	ch     rune // current character
	offset int  // character offset
	src    []rune
}

func (s *Scanner) skipWhitespace() {
	for s.ch == ' ' || s.ch == '\t' || s.ch == '\n' || s.ch == '\r' {
		if s.ch == eof {
			return
		}
		s.next()
	}
}

func (s *Scanner) Scan() (pos token.Pos, tok token.Token, lit string) {
	s.skipWhitespace()
	pos = token.Pos(s.offset)

	switch ch := s.ch; {
	case isLetter(ch):
		tok, lit = s.scanIdentifier()
		break
	case isDecimal(ch):
		tok, lit = s.scanNumber()
		break
	default:
		s.next()
		switch ch {
		case -1:
			tok = token.EOF
		case '(':
			tok = token.LPAREN
		case ')':
			tok = token.RPAREN
		case '{':
			tok = token.LBRACE
		case '}':
			tok = token.RBRACE
		case '+':
			tok = token.ADD
		case '-':
			tok = token.SUB
		case '*':
			tok = token.MUL
		case '/':
			tok = token.QUO
		case ',':
			tok = token.COMMA
		default:
			tok = token.ILLEGAL
			lit = string(ch)
		}
	}
	return
}

func (s *Scanner) scanNumber() (token.Token, string) {
	offs := s.offset
	tok := token.INT
	hasDot := false

	for isDecimal(s.ch) || s.ch == '.' {
		if s.ch == '.' {
			if hasDot == false {
				hasDot = true
				tok = token.FLOAT
			} else {
				tok = token.ILLEGAL
			}
		}
		s.next()
	}

	return tok, string(s.src[offs:s.offset])
}

func (s *Scanner) scanIdentifier() (token.Token, string) {
	offs := s.offset
	for isDecimal(s.ch) || isLetter(s.ch) || s.ch == '_' {
		s.next()
	}
	return token.IDENT, string(s.src[offs:s.offset])
}

func (s *Scanner) next() {
	if s.offset < len(s.src)-1 {
		s.offset++
		s.ch = s.src[s.offset]

	} else {
		s.ch = eof
		s.offset = len(s.src)
	}
}

func isLetter(ch rune) bool {
	return 'a' <= lower(ch) && lower(ch) <= 'z' || ch == '_' || ch >= utf8.RuneSelf && unicode.IsLetter(ch)
}

func lower(ch rune) rune     { return ('a' - 'A') | ch } // returns lower-case ch iff ch is ASCII letter
func isDecimal(ch rune) bool { return '0' <= ch && ch <= '9' }

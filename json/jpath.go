package json

import (
	"fmt"
	"go/token"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const eof = -1

type pathScanner struct {
	ch     rune // current character
	offset int  // character offset
	src    []rune
}

func (s *pathScanner) skipWhitespace() {
	for s.ch == ' ' || s.ch == '\t' || s.ch == '\n' || s.ch == '\r' {
		if s.ch == eof {
			return
		}
		s.next()
	}
}

func isLetter(ch rune) bool {
	return 'a' <= lower(ch) && lower(ch) <= 'z' || ch == '_' || ch >= utf8.RuneSelf && unicode.IsLetter(ch)
}

func lower(ch rune) rune { return ('a' - 'A') | ch } // returns lower-case ch iff ch is ASCII letter

func (s *pathScanner) Scan() (pos Pos, tok token.Token, lit string) {
	s.skipWhitespace()
	pos = Pos(s.offset)

	switch ch := s.ch; {
	case isLetter(ch):
		tok, lit = s.scanString()
		break
	case isDecimal(ch):
		tok, lit = s.scanNumber()
		break
	default:
		s.next()
		switch ch {
		case -1:
			tok = token.EOF
		case '-':
			tok = token.SUB
		case '.':
			tok = token.PERIOD
		case '$':
			tok = token.IDENT
		case '[':
			tok = token.LBRACK
		case ']':
			tok = token.RBRACK
		case ':':
			tok = token.COLON
		case '*':
			tok = token.MUL
		default:
			tok = token.ILLEGAL
			lit = string(ch)
		}
	}
	return
}

func (s *pathScanner) next() {
	if s.offset < len(s.src)-1 {
		s.offset++
		s.ch = s.src[s.offset]

	} else {
		s.ch = eof
		s.offset = len(s.src)
	}
}

func (s *pathScanner) scanString() (token.Token, string) {
	offs := s.offset
	for isDecimal(s.ch) || isLetter(s.ch) || s.ch == '_' {
		s.next()
	}
	return token.STRING, string(s.src[offs:s.offset])
}

func (s *pathScanner) scanNumber() (token.Token, string) {
	offs := s.offset
	tok := token.INT

	for isDecimal(s.ch) {
		s.next()
	}

	return tok, string(s.src[offs:s.offset])
}

func buildJsonPath(path string) ([]jsonPath, error) {
	scanner := &pathScanner{
		src:    []rune(path),
		offset: -1,
	}
	result := make([]jsonPath, 0)
	preToken := token.ILLEGAL
	for {
		_, tok, lit := scanner.Scan()
		switch tok {
		case token.EOF:
			return result, nil
		case token.STRING, token.INT:
			if preToken == token.ELLIPSIS {
				result = append(result, recursivePath(lit))
			} else {
				result = append(result, propertyPath(lit))
			}
			break
		case token.LBRACK:
			path, err := scanIndexedPath(scanner)
			if err != nil {
				return nil, err
			}
			result = append(result, path)
		case token.MUL:
			result = append(result, wildcardPath("*"))
			break
		case token.PERIOD:
			if preToken == token.PERIOD {
				preToken = token.ELLIPSIS
				continue
			}
		}
		preToken = tok
	}

}

func scanIndexedPath(scanner *pathScanner) (jsonPath, error) {
	path := strings.Builder{}
	path.WriteString("[")
	preToken := token.ILLEGAL
	slice := slicePath{}
	for {
		_, tok, lit := scanner.Scan()
		path.WriteString(lit)
		switch tok {
		case token.EOF:
			return nil, fmt.Errorf("invalid indexed path eof:%s", path.String())
		case token.INT:
			index, _ := strconv.Atoi(lit)
			if preToken == token.SUB {
				slice.AddNum(-index)
			} else {
				slice.AddNum(index)
			}
			break
		case token.COLON:
			slice.AddColon()
			break
		case token.RBRACK:
			// todo 这边还需要更精细的判定
			return slice.build(), nil
		case token.SUB:
			break

		default:
			return nil, fmt.Errorf("invalid indexed path:%s", path.String())
		}
		preToken = tok
	}
}

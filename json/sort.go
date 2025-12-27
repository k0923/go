package xjson

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	gsort "sort"
	"unicode/utf8"
)

type Pos int

type Token int

const (
	ILLEGAL Token = iota
	STRING        // "ABC"
	NUMBER        // 123.45
	IDENT         // true or false or null
	LBRACK        // [
	LBRACE        // {
	COMMA         // ,
	RBRACK        // ]
	RBRACE        // }
	COLON         // :
	DOT           // .
	DOLLAR        // $
	EOF
)

var tokens = [...]string{
	ILLEGAL: "ILLEGAL",
	STRING:  "STRING",
	NUMBER:  "NUMBER",
	IDENT:   "IDENT",
	LBRACK:  "[",
	LBRACE:  "{",
	COMMA:   ",",
	RBRACK:  "]",
	RBRACE:  "}",
	COLON:   ":",
	DOT:     ".",
	DOLLAR:  "$",
	EOF:     "EOF",
}

func (tok Token) String() string {
	if 0 <= tok && tok < Token(len(tokens)) {
		return tokens[tok]
	}
	return ""
}

func newScanner(data []rune) *scanner {
	sc := &scanner{
		src:    data,
		offset: -1,
	}
	sc.next()
	return sc
}

type scanner struct {
	eof    bool
	ch     rune
	src    []rune
	offset int
}

type POS [2]int

func (s *scanner) scan() (Token, POS, error) {
	s.skipWhitespace()
	start := s.offset
	var err error
	var token Token
	switch s.ch {
	case '1', '2', '3', '4', '5', '6', '7', '8', '9', '0':
		token = NUMBER
		err = s.scanNumber()
		return token, POS{start, s.offset}, err
	case 't', 'f', 'n':
		err = s.scanIdentity()
		token = IDENT
		return token, POS{start, s.offset}, err
	case ',':
		token = COMMA
	case ':':
		token = COLON
	case '{':
		token = LBRACE
	case '}':
		token = RBRACE
	case '[':
		token = LBRACK
	case ']':
		token = RBRACK
	case '"':
		token = STRING
		err = s.scanString()
	default:
		if s.eof {
			return EOF, POS{s.offset, s.offset}, nil
		}
		return ILLEGAL, POS{start, start}, errors.New("invalid character")
	}
	s.next()
	return token, POS{start, s.offset}, err
}

func (s *scanner) skipWhitespace() {

	for s.ch == ' ' || s.ch == '\t' || s.ch == '\n' || s.ch == '\r' {
		if s.eof {
			return
		}
		s.next()
	}
}

func (s *scanner) scanIdentity() error {
	data := make([]rune, 0)
	switch s.ch {
	case 'f':
		for i := 0; i < 5; i++ {
			data = append(data, s.ch)
			s.next()
		}
		if string(data) == "false" {
			return nil
		} else {
			return errors.New("invalid bool data")
		}
	case 't':
		for i := 0; i < 4; i++ {
			data = append(data, s.ch)
			s.next()
		}
		if string(data) == "true" {
			return nil
		} else {
			return errors.New("invalid bool data")
		}
	default:
		for i := 0; i < 4; i++ {
			data = append(data, s.ch)
			s.next()
		}
		if string(data) == "null" {
			return nil
		} else {
			return errors.New("invalid null data")
		}
	}
}

func (s *scanner) scanString() error {
	var pre rune
	s.next()
	for {
		pre = s.ch
		if s.eof {
			return errors.New("string value is not valid")
		}
		if s.ch == '"' && pre != '\\' {
			return nil
		}
		s.next()
	}
}

func (s *scanner) scanNumber() error {
	hasDot := false
	for isDecimal(s.ch) || s.ch == '.' {
		if s.ch == '.' {
			if !hasDot {
				hasDot = true
			} else {
				return errors.New("number is not valid")
			}
		}
		s.next()
	}
	return nil
}

func (s *scanner) next() {
	if s.offset < len(s.src)-1 {
		s.offset++
		s.ch = s.src[s.offset]
	} else {
		s.eof = true
		s.offset = len(s.src)
	}
}

func isDecimal(ch rune) bool { return '0' <= ch && ch <= '9' }

type writer interface {
	Write(io.Writer, []rune) error
}

type kvPair struct {
	key   []rune
	value writer
}

func (kvp *kvPair) Write(writer io.Writer, src []rune) error {
	if _, err := writer.Write([]byte(string(append(kvp.key, ':')))); err != nil {
		return err
	}
	return kvp.value.Write(writer, src)
}

type object struct {
	pairs []*kvPair
}

func (obj *object) Write(writer io.Writer, src []rune) error {
	gsort.SliceStable(obj.pairs, func(i, j int) bool {
		return string(obj.pairs[i].key) < string(obj.pairs[j].key)
	})
	if _, err := writer.Write([]byte{'{'}); err != nil {
		return err
	}
	for i, pair := range obj.pairs {
		if err := pair.Write(writer, src); err != nil {
			return err
		}
		if i < len(obj.pairs)-1 {
			if _, err := writer.Write([]byte{','}); err != nil {
				return err
			}
		}
	}
	_, err := writer.Write([]byte{'}'})
	return err
}

type array struct {
	items []writer
}

func (array *array) Write(writer io.Writer, src []rune) error {
	if _, err := writer.Write([]byte{'['}); err != nil {
		return err
	}
	for i, item := range array.items {
		if err := item.Write(writer, src); err != nil {
			return err
		}
		if i < len(array.items)-1 {
			if _, err := writer.Write([]byte{','}); err != nil {
				return err
			}
		}
	}
	_, err := writer.Write([]byte{']'})
	return err
}

type field struct {
	pos POS
}

func (field *field) Write(writer io.Writer, src []rune) error {
	_, err := writer.Write([]byte(string(src[field.pos[0]:field.pos[1]])))
	return err
}

type jsonParser struct {
	token Token
	pos   POS
	s     *scanner
	eof   bool
}

func (parser *jsonParser) Parse(data []rune) (writer, error) {
	parser.s = newScanner(data)
	if err := parser.next(); err != nil {
		return nil, err
	}
	switch parser.token {
	case LBRACE:
		return parser.parseObj()
	case LBRACK:
		return parser.parseArray()
	default:
		return nil, fmt.Errorf("invalid json token:%d", parser.token)
	}
}

func (parser *jsonParser) parsePair() (*kvPair, error) {
	if err := parser.next(); err != nil {
		return nil, err
	}

	if parser.token == RBRACE {
		return nil, nil
	}

	if parser.token != STRING {
		return nil, fmt.Errorf("invalid string token found:%s", parser.token)
	}

	pair := kvPair{}

	pair.key = parser.s.src[parser.pos[0]:parser.pos[1]]
	if err := parser.next(); err != nil {
		return nil, err
	}
	if parser.token != COLON {
		return nil, fmt.Errorf("invalid colon token found:%s", parser.token)
	}
	if err := parser.next(); err != nil {
		return nil, err
	}
	switch parser.token {
	case STRING, NUMBER, IDENT:
		pair.value = &field{pos: parser.pos}
	case LBRACE:
		if w, err := parser.parseObj(); err != nil {
			return nil, err
		} else {
			pair.value = w
		}
	case LBRACK:
		if w, err := parser.parseArray(); err != nil {
			return nil, err
		} else {
			pair.value = w
		}
	default:
		return nil, fmt.Errorf("invalid token found:%s", parser.token)
	}
	return &pair, nil
}

func (parser *jsonParser) parseObj() (writer, error) {
	object := object{
		pairs: make([]*kvPair, 0),
	}

	for {
		pair, err := parser.parsePair()
		if err != nil {
			return nil, err
		}
		if pair == nil {
			return &object, nil
		}
		object.pairs = append(object.pairs, pair)
		if err := parser.next(); err != nil {
			return nil, err
		}
		switch parser.token {
		case COMMA:
			continue
		case RBRACE:
			return &object, nil
		default:
			return nil, fmt.Errorf("invalid token in object:%s", parser.token)
		}
	}
}

func (parser *jsonParser) parseArray() (writer, error) {

	array := array{items: make([]writer, 0)}

	for {
		if err := parser.next(); err != nil {
			return nil, err
		}

		switch parser.token {
		case STRING, IDENT, NUMBER:
			array.items = append(array.items, &field{pos: parser.pos})
		case LBRACE:
			if w, err := parser.parseObj(); err != nil {
				return nil, err
			} else {
				array.items = append(array.items, w)
			}
		case LBRACK:
			if w, err := parser.parseArray(); err != nil {
				return nil, err
			} else {
				array.items = append(array.items, w)
			}
		case RBRACK:
			return &array, nil
		default:
			return nil, fmt.Errorf("invalid array token found:%s", parser.token)
		}

		if err := parser.next(); err != nil {
			return nil, err
		}
		switch parser.token {
		case COMMA:
			continue
		case RBRACK:
			return &array, nil
		default:
			return nil, fmt.Errorf("invalid array1 token found:%s", parser.token)
		}
	}
}

func (parser *jsonParser) next() error {
	if parser.eof {
		return nil
	}
	tok, pos, err := parser.s.scan()
	if err != nil {
		return err
	}

	parser.token = tok
	parser.pos = pos
	parser.eof = parser.s.eof
	return nil
}

// type tokenInfo struct {
// 	token Token
// 	pos   POS
// }

// type jsonObj struct {
// 	key   []byte
// 	value []byte
// }

// type jsonSort struct {
// 	token Token
// 	pos   POS
// 	s     *scanner
// 	src   []byte
// }

// func (sort *jsonSort) sort() ([]byte, error) {
// 	if err := sort.next(); err != nil {
// 		return nil, err
// 	}
// 	switch sort.token {
// 	case LBRACE:
// 		return sort.sortObj()
// 	case LBRACK:
// 		return sort.sortArray()
// 	default:
// 		return nil, fmt.Errorf("invalid json token:%d", sort.token)
// 	}
// }

// func (sort *jsonSort) scanPair() (*jsonObj, error) {
// 	if err := sort.next(); err != nil {
// 		return nil, err
// 	}
// 	if sort.token != STRING {
// 		fmt.Println(sort.token)
// 		return nil, errors.New("invalid key found")
// 	}
// 	obj := jsonObj{
// 		key: sort.src[sort.pos[0]:sort.pos[1]],
// 	}
// 	if err := sort.next(); err != nil {
// 		return nil, err
// 	}
// 	if sort.token != COLON {
// 		fmt.Println(sort.token)
// 		return nil, errors.New("invalid colon")
// 	}
// 	if err := sort.next(); err != nil {
// 		return nil, err
// 	}
// 	switch sort.token {
// 	case STRING, NUMBER, IDENT:
// 		obj.value = sort.src[sort.pos[0]:sort.pos[1]]
// 	case LBRACE:
// 		if data, err := sort.sortObj(); err != nil {
// 			return nil, err
// 		} else {
// 			obj.value = data
// 		}
// 	case LBRACK:
// 		if data, err := sort.sortArray(); err != nil {
// 			return nil, err
// 		} else {
// 			obj.value = data
// 		}
// 	default:
// 		return nil, errors.New("invalid token found")
// 	}
// 	return &obj, nil
// }

// func (sort *jsonSort) sortObj() ([]byte, error) {
// 	objs := make([]jsonObj, 0)
// 	for {
// 		pair, err := sort.scanPair()
// 		if err != nil {
// 			return nil, err
// 		}
// 		objs = append(objs, *pair)
// 		if err := sort.next(); err != nil {
// 			return nil, err
// 		}
// 		switch sort.token {
// 		case COMMA:
// 			continue
// 		case RBRACE:
// 			gsort.SliceStable(objs, func(i, j int) bool {
// 				return string(objs[i].key) < string(objs[j].key)
// 			})
// 			result := []byte{'{'}
// 			for i, obj := range objs {
// 				result = append(result, obj.key...)
// 				result = append(result, ':')
// 				result = append(result, obj.value...)
// 				if i < len(objs)-1 {
// 					result = append(result, ',')
// 				}
// 			}
// 			result = append(result, '}')
// 			return result, nil
// 		default:
// 			return nil, errors.New("invalid token")
// 		}
// 	}
// }

// func (sort *jsonSort) sortArray() ([]byte, error) {
// 	result := []byte{'['}
// 	for {
// 		if err := sort.next(); err != nil {
// 			return nil, err
// 		}
// 		switch sort.token {
// 		case STRING, IDENT, NUMBER:
// 			result = append(result, sort.src[sort.pos[0]:sort.pos[1]]...)
// 		case LBRACE:
// 			if data, err := sort.sortObj(); err != nil {
// 				return nil, err
// 			} else {
// 				result = append(result, data...)
// 			}
// 		case LBRACK:
// 			if data, err := sort.sortArray(); err != nil {
// 				return nil, err
// 			} else {
// 				result = append(result, data...)
// 			}
// 		default:
// 			return nil, errors.New("invalid array token")
// 		}
// 		if err := sort.next(); err != nil {
// 			return nil, err
// 		}
// 		switch sort.token {
// 		case COMMA:
// 			result = append(result, ',')
// 			continue
// 		case RBRACK:
// 			result = append(result, ']')
// 			return result, nil
// 		default:
// 			return nil, errors.New("invalid array split token")
// 		}
// 	}
// }

// func (sort *jsonSort) next() error {
// 	if sort.s.eof == true {
// 		return nil
// 	}
// 	tok, pos, err := sort.s.scan()
// 	if err != nil {
// 		return err
// 	}
// 	sort.token = tok
// 	sort.pos = pos
// 	return nil
// }

func SortJSON(data []byte) ([]byte, error) {
	parse := jsonParser{}

	runes := make([]rune, 0, len(data))
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		runes = append(runes, r)
		data = data[size:]
	}

	w, err := parse.Parse(runes)
	if err != nil {
		return nil, err
	}
	newBytes := make([]byte, 0, len(data))
	buf := bytes.NewBuffer(newBytes)
	if err := w.Write(buf, runes); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

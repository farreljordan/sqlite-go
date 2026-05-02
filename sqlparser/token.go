package sqlparser

import (
	"strings"
	"unicode"
)

// TokenType identifies what kind of token was scanned.
type TokenType int

const (
	// single-char punctuation
	TOKEN_STAR   TokenType = iota // *
	TOKEN_COMMA                   // ,
	TOKEN_LPAREN                  // (
	TOKEN_RPAREN                  // )
	TOKEN_EQ                      // =

	// literals / identifiers
	TOKEN_IDENT  // bare word or quoted identifier
	TOKEN_STRING // 'string literal'

	TOKEN_SELECT
	TOKEN_FROM
	TOKEN_WHERE

	TOKEN_EOF
	TOKEN_ILLEGAL
)

// Token is a single unit returned by the tokenizer.
type Token struct {
	Type TokenType
	Val  string // original text
}

// Tokenizer holds the scanning state.
type Tokenizer struct {
	input []rune
	pos   int
}

func NewTokenizer(sql string) *Tokenizer {
	return &Tokenizer{input: []rune(strings.TrimSpace(sql))}
}

func (t *Tokenizer) Next() Token {
	t.skipWhitespace()

	if t.pos >= len(t.input) {
		return Token{Type: TOKEN_EOF}
	}

	ch := t.input[t.pos]

	switch ch {
	case '*':
		t.pos++
		return Token{Type: TOKEN_STAR, Val: "*"}
	case ',':
		t.pos++
		return Token{Type: TOKEN_COMMA, Val: ","}
	case '(':
		t.pos++
		return Token{Type: TOKEN_LPAREN, Val: "("}
	case ')':
		t.pos++
		return Token{Type: TOKEN_RPAREN, Val: ")"}
	case '=':
		t.pos++
		return Token{Type: TOKEN_EQ, Val: "="}
	case '\'', '"', '`':
		return t.scanQuoted(ch)
	case '[':
		return t.scanBracket()
	}

	if unicode.IsLetter(ch) || ch == '_' {
		return t.scanWord()
	}

	t.pos++
	return Token{Type: TOKEN_ILLEGAL, Val: string(ch)}
}

func (t *Tokenizer) skipWhitespace() {
	for t.pos < len(t.input) && unicode.IsSpace(t.input[t.pos]) {
		t.pos++
	}
}

// scanWord reads a bare identifier or keyword.
func (t *Tokenizer) scanWord() Token {
	start := t.pos
	for t.pos < len(t.input) && (unicode.IsLetter(t.input[t.pos]) || unicode.IsDigit(t.input[t.pos]) || t.input[t.pos] == '_') {
		t.pos++
	}
	word := string(t.input[start:t.pos])
	return Token{Type: keywordOrIdent(word), Val: word}
}

// scanQuoted reads a "quoted", 'quoted', or `quoted` identifier/string.
func (t *Tokenizer) scanQuoted(quote rune) Token {
	t.pos++ // consume opening quote
	start := t.pos
	for t.pos < len(t.input) && t.input[t.pos] != quote {
		t.pos++
	}
	val := string(t.input[start:t.pos])
	if t.pos < len(t.input) {
		t.pos++ // consume closing quote
	}
	if quote == '\'' {
		return Token{Type: TOKEN_STRING, Val: val}
	}
	return Token{Type: TOKEN_IDENT, Val: val}
}

func (t *Tokenizer) scanBracket() Token {
	t.pos++ // consume '['
	start := t.pos
	for t.pos < len(t.input) && t.input[t.pos] != ']' {
		t.pos++
	}
	val := string(t.input[start:t.pos])
	if t.pos < len(t.input) {
		t.pos++ // consume ']'
	}
	return Token{Type: TOKEN_IDENT, Val: val}
}

func keywordOrIdent(word string) TokenType {
	switch strings.ToUpper(word) {
	case "SELECT":
		return TOKEN_SELECT
	case "FROM":
		return TOKEN_FROM
	case "WHERE":
		return TOKEN_WHERE
	default:
		return TOKEN_IDENT
	}
}

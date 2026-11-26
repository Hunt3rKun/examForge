// Package inline implements the deterministic tokenizer for the Exam IR inline
// markup subset: $math$, **bold**, *italic*, __underline__ and {{blank:N}}.
//
// Parse never returns a fatal error. Malformed markup is kept as visible text
// and reported through Diagnostics, whose offsets are UTF-8 byte offsets into
// the original input.
package inline

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Token types produced by Parse.
const (
	TypeText      = "text"
	TypeMath      = "math_inline"
	TypeBold      = "bold"
	TypeItalic    = "italic"
	TypeUnderline = "underline"
	TypeBlank     = "blank"
)

// Diagnostic codes reported by Parse.
const (
	DiagInvalidUTF8       = "invalid_utf8"
	DiagInvalidBlank      = "invalid_blank"
	DiagUnclosedMath      = "unclosed_math"
	DiagUnclosedBold      = "unclosed_bold"
	DiagUnclosedItalic    = "unclosed_italic"
	DiagUnclosedUnderline = "unclosed_underline"
	DiagEmptySpan         = "empty_span"
)

// Token is one inline fragment. Text carries plain text, LaTeX or styled text;
// Index is the positive N of {{blank:N}}.
type Token struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Index int    `json:"index,omitempty"`
}

// Diagnostic reports degraded markup. Offsets are 0-based UTF-8 byte offsets
// into the original input; EndByte is exclusive.
type Diagnostic struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
}

// Result is the output of Parse. Tokens is never nil.
type Result struct {
	Tokens      []Token      `json:"tokens"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

const (
	blankPrefix     = "{{blank:"
	maxBlankBytes   = 64
	replacementRune = "�"
)

// Parse tokenizes source. It never panics and never returns a fatal error:
// invalid UTF-8 is replaced by U+FFFD, and malformed markup is kept as text.
func Parse(source string) Result {
	cleaned, rawAt, diags := sanitize(source)
	p := &parser{s: cleaned, rawAt: rawAt, res: Result{Tokens: []Token{}, Diagnostics: diags}}
	p.run()
	p.flush()
	sort.SliceStable(p.res.Diagnostics, func(i, j int) bool {
		return p.res.Diagnostics[i].StartByte < p.res.Diagnostics[j].StartByte
	})
	return p.res
}

// sanitize replaces every invalid UTF-8 byte with U+FFFD. rawAt maps each
// cleaned byte offset back to the original input offset (len == len(cleaned)+1).
func sanitize(s string) (cleaned string, rawAt []int, diags []Diagnostic) {
	var b strings.Builder
	rawAt = make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || size != 1 {
			b.WriteString(s[i : i+size])
			for k := 0; k < size; k++ {
				rawAt = append(rawAt, i)
			}
			i += size
			continue
		}
		start := i
		for i < len(s) {
			r, size = utf8.DecodeRuneInString(s[i:])
			if r != utf8.RuneError || size != 1 {
				break
			}
			b.WriteString(replacementRune)
			for k := 0; k < 3; k++ {
				rawAt = append(rawAt, i)
			}
			i++
		}
		diags = append(diags, Diagnostic{
			Code:      DiagInvalidUTF8,
			Message:   fmt.Sprintf("%d invalid UTF-8 byte(s) replaced with U+FFFD", i-start),
			StartByte: start,
			EndByte:   i,
		})
	}
	rawAt = append(rawAt, len(s))
	return b.String(), rawAt, diags
}

type parser struct {
	s     string // cleaned text: valid UTF-8
	rawAt []int
	i     int
	buf   strings.Builder // pending plain text, or the current style segment
	res   Result
}

func isEscapable(c byte) bool { return c == '\\' || c == '$' || c == '*' || c == '_' }

func (p *parser) width(i int) int {
	_, size := utf8.DecodeRuneInString(p.s[i:])
	return size
}

func (p *parser) diag(code string, i, j int, format string, args ...any) {
	p.res.Diagnostics = append(p.res.Diagnostics, Diagnostic{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		StartByte: p.rawAt[i],
		EndByte:   p.rawAt[j],
	})
}

// emitAs appends a token holding the pending text. Callers decide whether an
// empty segment is meaningful; style segments around math use emitStyleText.
func (p *parser) emitAs(typ string) {
	p.res.Tokens = append(p.res.Tokens, Token{Type: typ, Text: p.buf.String()})
	p.buf.Reset()
}

// emitStyleText drops empty style fragments around inline math. Besides being
// redundant, an empty Text is omitted from JSON and must not reach the template.
func (p *parser) emitStyleText(typ string) {
	if p.buf.Len() > 0 {
		p.emitAs(typ)
	}
}

// flush appends the pending plain text as one token unless it is empty.
func (p *parser) flush() {
	if p.buf.Len() > 0 {
		p.emitAs(TypeText)
	}
}

func (p *parser) run() {
	s := p.s
	for p.i < len(s) {
		i := p.i
		switch c := s[i]; {
		case c == '\\':
			if i+1 < len(s) && isEscapable(s[i+1]) {
				p.buf.WriteByte(s[i+1])
				p.i = i + 2
			} else {
				p.buf.WriteByte(c)
				p.i = i + 1
			}
		case c == '{' && strings.HasPrefix(s[i:], blankPrefix):
			p.blank(i)
		case c == '*' && i+1 < len(s) && s[i+1] == '*':
			p.styleSpan(i, TypeBold, DiagUnclosedBold)
		case c == '_' && i+1 < len(s) && s[i+1] == '_':
			p.styleSpan(i, TypeUnderline, DiagUnclosedUnderline)
		case c == '$':
			p.mathSpan(i)
		case c == '*':
			p.styleSpan(i, TypeItalic, DiagUnclosedItalic)
		default:
			w := p.width(i)
			p.buf.WriteString(s[i : i+w])
			p.i = i + w
		}
	}
}

// mathClose returns the index of the earliest unescaped '$' at or after from,
// or -1. Backslashes are literal inside math, so only an unescaped '$' closes.
func mathClose(s string, from int) int {
	for j := from; j < len(s); {
		if s[j] == '\\' && j+1 < len(s) {
			j += 2
			continue
		}
		if s[j] == '$' {
			return j
		}
		_, size := utf8.DecodeRuneInString(s[j:])
		j += size
	}
	return -1
}

func (p *parser) mathSpan(i int) {
	s := p.s
	m := mathClose(s, i+1)
	switch {
	case m < 0:
		p.diag(DiagUnclosedMath, i, i+1, "unclosed math: no closing '$' after '%s'", s[i:i+1])
		p.buf.WriteByte('$')
		p.i = i + 1
	case m == i+1:
		p.diag(DiagEmptySpan, i, m+1, "empty math span %q kept as text", s[i:m+1])
		p.buf.WriteString(s[i : m+1])
		p.i = m + 1
	default:
		p.flush()
		p.res.Tokens = append(p.res.Tokens, Token{Type: TypeMath, Text: s[i+1 : m]})
		p.i = m + 1
	}
}

// styleSpan handles a style opener at i (a run of one or two identical
// delimiter bytes). The closer is the earliest unescaped identical delimiter;
// $...$ inside the span is the only nested markup and interrupts the style.
func (p *parser) styleSpan(i int, typ, unclosed string) {
	s := p.s
	n := len(s)
	d := s[i]
	l := 1
	if i+1 < n && s[i+1] == d {
		l = 2
	}
	// Pass 1: find the closer, skipping over $...$ math.
	j, inMath, closer := i+l, false, -1
	for j < n {
		c := s[j]
		if inMath {
			if c == '\\' && j+1 < n {
				j += 2
				continue
			}
			if c == '$' {
				inMath = false
			}
		} else {
			if c == '\\' && j+1 < n && isEscapable(s[j+1]) {
				j += 2
				continue
			}
			if c == '$' {
				inMath = true
			} else if c == d {
				// A "**" run is never treated as two single "*" delimiters,
				// neither as a bold closer of one star nor as an italic closer.
				if l == 2 {
					if j+1 < n && s[j+1] == d {
						closer = j
						break
					}
				} else if d == '*' && j+1 < n && s[j+1] == '*' {
					j += 2
					continue
				} else {
					closer = j
					break
				}
			}
		}
		j += p.width(j)
	}
	switch {
	case closer < 0:
		p.diag(unclosed, i, i+l, "unclosed %s: no closing %q before end of text", typ, s[i:i+l])
		p.buf.WriteString(s[i : i+l])
		p.i = i + l
	case closer == i+l:
		p.diag(DiagEmptySpan, i, closer+l, "empty %s span %q kept as text", typ, s[i:closer+l])
		p.buf.WriteString(s[i : closer+l])
		p.i = closer + l
	default:
		p.flush()
		p.emitStyleSpan(i, l, closer, typ)
		p.i = closer + l
	}
}

// emitStyleSpan emits the content of a closed style span [i+l, closer) as
// alternating style tokens and math tokens. Backslash escapes are resolved in
// style text; math content is passed through verbatim.
func (p *parser) emitStyleSpan(i, l, closer int, typ string) {
	s := p.s
	for k := i + l; k < closer; {
		c := s[k]
		switch {
		case c == '\\' && k+1 < closer && isEscapable(s[k+1]):
			p.buf.WriteByte(s[k+1])
			k += 2
		case c == '$':
			m := mathClose(s, k+1)
			if m < 0 || m >= closer {
				// Pass 1 guarantees a closed math span; keep the marker literal otherwise.
				p.buf.WriteByte(c)
				k++
				continue
			}
			if m == k+1 {
				p.diag(DiagEmptySpan, k, m+1, "empty math span %q kept as text", s[k:m+1])
				p.buf.WriteString(s[k : m+1])
			} else {
				p.emitStyleText(typ)
				p.res.Tokens = append(p.res.Tokens, Token{Type: TypeMath, Text: s[k+1 : m]})
			}
			k = m + 1
		default:
			w := p.width(k)
			p.buf.WriteString(s[k : k+w])
			k += w
		}
	}
	p.emitStyleText(typ)
}

// blank handles a candidate at i that starts with "{{blank:". Only
// {{blank:N}} with a positive decimal N is a token; anything else is kept as
// text with an invalid_blank diagnostic.
func (p *parser) blank(i int) {
	s := p.s
	j := i + len(blankPrefix)
	limit := min(len(s), i+maxBlankBytes)
	for j < limit && s[j] != '}' && s[j] != '\n' && s[j] != '\r' {
		j++
	}
	end := j
	if j+1 < len(s) && s[j] == '}' && s[j+1] == '}' {
		end = j + 2
	}
	candidate := s[i:end]
	if n, ok := parseBlank(candidate); ok {
		p.flush()
		p.res.Tokens = append(p.res.Tokens, Token{Type: TypeBlank, Index: n})
		p.i = end
		return
	}
	p.diag(DiagInvalidBlank, i, end, "invalid blank %q: expected {{blank:N}} with N a positive integer", candidate)
	p.buf.WriteString(candidate)
	p.i = end
}

func parseBlank(candidate string) (int, bool) {
	if !strings.HasPrefix(candidate, blankPrefix) || !strings.HasSuffix(candidate, "}}") {
		return 0, false
	}
	digits := candidate[len(blankPrefix) : len(candidate)-2]
	if digits == "" {
		return 0, false
	}
	for k := 0; k < len(digits); k++ {
		if digits[k] < '0' || digits[k] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n == 0 || n > math.MaxInt {
		return 0, false
	}
	return int(n), true
}

package inline

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func T(s string) Token { return Token{Type: TypeText, Text: s} }
func M(s string) Token { return Token{Type: TypeMath, Text: s} }
func B(s string) Token { return Token{Type: TypeBold, Text: s} }
func I(s string) Token { return Token{Type: TypeItalic, Text: s} }
func U(s string) Token { return Token{Type: TypeUnderline, Text: s} }
func N(n int) Token    { return Token{Type: TypeBlank, Index: n} }
func d(code string, a, b int) Diagnostic {
	return Diagnostic{Code: code, StartByte: a, EndByte: b}
}

var cases = []struct {
	name  string
	in    string
	want  []Token
	diags []Diagnostic
}{
	// Every token type, merging and adjacency.
	{"empty input", "", []Token{}, nil},
	{"plain text", "hello world", []Token{T("hello world")}, nil},
	{"inline math", "已知 $a_n = 1$ 求", []Token{T("已知 "), M("a_n = 1"), T(" 求")}, nil},
	{"bold", "**重点**", []Token{B("重点")}, nil},
	{"italic", "前 *强调* 后", []Token{T("前 "), I("强调"), T(" 后")}, nil},
	{"underline", "__下划线__", []Token{U("下划线")}, nil},
	{"blank", "第{{blank:1}}空", []Token{T("第"), N(1), T("空")}, nil},
	{"adjacent tokens never merge", "**a****b**", []Token{B("a"), B("b")}, nil},
	{"adjacent math tokens never merge", "$a$$b$", []Token{M("a"), M("b")}, nil},
	{"text between tokens merges once", "a$b$c", []Token{T("a"), M("b"), T("c")}, nil},
	{"double star is never two italics", "*a **b** c*", []Token{I("a **b** c")}, nil},
	{"bold opener beats italic", "x**y**z", []Token{T("x"), B("y"), T("z")}, nil},
	{"single underscore is text", "a_b_c", []Token{T("a_b_c")}, nil},
	{"unicode braces are text", "{{title}}", []Token{T("{{title}}")}, nil},

	// Escapes.
	{"escape dollar opens no math", `a \$5 b`, []Token{T("a $5 b")}, nil},
	{"escaped dollar then real math", `\$$x$`, []Token{T("$"), M("x")}, nil},
	{"escape star opens no italic", `\*x\*`, []Token{T("*x*")}, nil},
	{"escape underscore opens no underline", `\_y\_`, []Token{T("_y_")}, nil},
	{"escape backslash", `\\`, []Token{T(`\`)}, nil},
	{"escaped backslash then italic", `\\*x*`, []Token{T(`\`), I("x")}, nil},
	{"unknown escape keeps backslash", `a\nb`, []Token{T(`a\nb`)}, nil},
	{"trailing backslash", `abc\`, []Token{T(`abc\`)}, nil},
	{"latex backslash stays in math", `$\alpha+\beta$`, []Token{M(`\alpha+\beta`)}, nil},
	{"latex fraction stays in math", `$\frac{1}{2}$`, []Token{M(`\frac{1}{2}`)}, nil},
	{"escaped dollar inside math stays latex", `$\$5$`, []Token{M(`\$5`)}, nil},
	{"escape resolved inside style", `**a\*b**`, []Token{B("a*b")}, nil},
	{"escape keeps style from closing early", `**a\** b**`, []Token{B("a** b")}, nil},

	// Unclosed markup degrades to text and scanning continues.
	{"unclosed math", "a $x", []Token{T("a $x")}, []Diagnostic{d(DiagUnclosedMath, 2, 3)}},
	{"escaped dollar is not a closer", `a \$b$ c`, []Token{T("a $b$ c")}, []Diagnostic{d(DiagUnclosedMath, 5, 6)}},
	{"unclosed bold", "a **x", []Token{T("a **x")}, []Diagnostic{d(DiagUnclosedBold, 2, 4)}},
	{"unclosed italic", "a *x", []Token{T("a *x")}, []Diagnostic{d(DiagUnclosedItalic, 2, 3)}},
	{"unclosed underline", "a __x", []Token{T("a __x")}, []Diagnostic{d(DiagUnclosedUnderline, 2, 4)}},
	{"unclosed math does not swallow later markup", "a $x **c**", []Token{T("a $x "), B("c")}, []Diagnostic{d(DiagUnclosedMath, 2, 3)}},
	{"unclosed bold does not swallow later markup", "**a *b*", []Token{T("**a "), I("b")}, []Diagnostic{d(DiagUnclosedBold, 0, 2)}},
	{"italic does not close inside a double star", "*a**", []Token{T("*a**")}, []Diagnostic{d(DiagUnclosedItalic, 0, 1), d(DiagUnclosedBold, 2, 4)}},
	{"italic closes at a lone star", "*a* b", []Token{I("a"), T(" b")}, nil},

	// Empty spans stay literal.
	{"empty math", "a$$b", []Token{T("a$$b")}, []Diagnostic{d(DiagEmptySpan, 1, 3)}},
	{"empty bold", "****", []Token{T("****")}, []Diagnostic{d(DiagEmptySpan, 0, 4)}},
	{"empty underline", "____", []Token{T("____")}, []Diagnostic{d(DiagEmptySpan, 0, 4)}},
	{"lone double star is unclosed bold", "**", []Token{T("**")}, []Diagnostic{d(DiagUnclosedBold, 0, 2)}},
	{"lone star is unclosed italic", "*", []Token{T("*")}, []Diagnostic{d(DiagUnclosedItalic, 0, 1)}},

	// Math inside a style interrupts the style token.
	{"math in style trailing", "**已知 $a_n = 1$**", []Token{B("已知 "), M("a_n = 1")}, nil},
	{"math only in style", "**$x$**", []Token{M("x")}, nil},
	{"math starts italic", "*$a$b*", []Token{M("a"), I("b")}, nil},
	{"math ends underline", "__a$x$__", []Token{U("a"), M("x")}, nil},
	{"math in style middle", "**x $y$ z**", []Token{B("x "), M("y"), B(" z")}, nil},
	{"two math in style", "*a $x$ b $y$ c*", []Token{I("a "), M("x"), I(" b "), M("y"), I(" c")}, nil},
	{"math protects style delimiter", "*x $a*b$ y*", []Token{I("x "), M("a*b"), I(" y")}, nil},
	{"empty math in style", "**a $$ b**", []Token{B("a $$ b")}, []Diagnostic{d(DiagEmptySpan, 4, 6)}},

	// Unsupported nesting stays literal.
	{"style in style", "**a *b* c**", []Token{B("a *b* c")}, nil},
	{"underline in bold", "**a __b__ c**", []Token{B("a __b__ c")}, nil},
	{"markup inside math is latex", "$a **b** \\text{c}$", []Token{M(`a **b** \text{c}`)}, nil},

	// Blanks.
	{"blank larger legal number", "{{blank:2147483647}}", []Token{N(2147483647)}, nil},
	{"blank leading zeros", "{{blank:007}}", []Token{N(7)}, nil},
	{"blank zero", "{{blank:0}}", []Token{T("{{blank:0}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 11)}},
	{"blank negative", "{{blank:-1}}", []Token{T("{{blank:-1}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 12)}},
	{"blank plus sign", "{{blank:+1}}", []Token{T("{{blank:+1}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 12)}},
	{"blank non digits", "{{blank:abc}}", []Token{T("{{blank:abc}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 13)}},
	{"blank space", "{{blank: 1}}", []Token{T("{{blank: 1}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 12)}},
	{"blank overflow", "{{blank:99999999999999999999}}", []Token{T("{{blank:99999999999999999999}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 30)}},
	{"blank missing close", "{{blank:1", []Token{T("{{blank:1")}, []Diagnostic{d(DiagInvalidBlank, 0, 9)}},
	{"blank single close brace", "{{blank:1}", []Token{T("{{blank:1}")}, []Diagnostic{d(DiagInvalidBlank, 0, 9)}},
	{"blank close on next line", "{{blank:1\n}}", []Token{T("{{blank:1\n}}")}, []Diagnostic{d(DiagInvalidBlank, 0, 9)}},
	{"blank keeps following markup", "{{blank:0}}$x$", []Token{T("{{blank:0}}"), M("x")}, []Diagnostic{d(DiagInvalidBlank, 0, 11)}},
	{"only math interrupts a style", "**答{{blank:3}}**", []Token{B("答{{blank:3}}")}, nil},

	// Unicode.
	{"cjk byte offsets", "函数$x$", []Token{T("函数"), M("x")}, nil},
	{"emoji", "😀$x$😀", []Token{T("😀"), M("x"), T("😀")}, nil},
	{"combining mark", "é*x*", []Token{T("é"), I("x")}, nil},
	{"marker next to unicode", "**中文**", []Token{B("中文")}, nil},
	{"fullwidth parens are text", "（　　）", []Token{T("（　　）")}, nil},
	{"cjk unclosed offset", "函数 $x", []Token{T("函数 $x")}, []Diagnostic{d(DiagUnclosedMath, 7, 8)}},
	{"invalid utf8", "a\xffb", []Token{T("a�b")}, []Diagnostic{d(DiagInvalidUTF8, 1, 2)}},
	{"invalid utf8 run", "a\xff\xfeb", []Token{T("a��b")}, []Diagnostic{d(DiagInvalidUTF8, 1, 3)}},
	{"offsets stay in raw bytes", "\xff **a", []Token{T("� **a")}, []Diagnostic{d(DiagInvalidUTF8, 0, 1), d(DiagUnclosedBold, 2, 4)}},
	{"truncated multibyte", "中\xe4", []Token{T("中�")}, []Diagnostic{d(DiagInvalidUTF8, 3, 4)}},
	{"encoded replacement char is valid", "a�b", []Token{T("a�b")}, nil},
}

func TestParse(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in)
			if len(got.Tokens) == 0 && len(tc.want) == 0 {
				// both empty
			} else if !equalTokens(got.Tokens, tc.want) {
				t.Fatalf("tokens\n got: %#v\nwant: %#v", got.Tokens, tc.want)
			}
			if got.Tokens == nil {
				t.Fatalf("Tokens must never be nil")
			}
			if !equalDiags(got.Diagnostics, tc.diags) {
				t.Fatalf("diagnostics\n got: %s\nwant: %s", formatDiags(got.Diagnostics), formatDiags(tc.diags))
			}
			for _, dg := range got.Diagnostics {
				if dg.Message == "" {
					t.Errorf("diagnostic %s has empty message", dg.Code)
				}
				if dg.StartByte < 0 || dg.EndByte > len(tc.in) || dg.StartByte > dg.EndByte {
					t.Errorf("diagnostic %s offsets [%d,%d) outside input of %d bytes", dg.Code, dg.StartByte, dg.EndByte, len(tc.in))
				}
			}
			for _, tok := range got.Tokens {
				if tok.Type != TypeBlank && tok.Text == "" {
					t.Errorf("empty %s token emitted", tok.Type)
				}
			}
			// Deterministic: the same input yields the identical result.
			if again := Parse(tc.in); !equalTokens(again.Tokens, got.Tokens) || !equalDiags(again.Diagnostics, got.Diagnostics) {
				t.Fatalf("Parse is not deterministic")
			}
		})
	}
}

func equalTokens(a, b []Token) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalDiags(a, b []Diagnostic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code || a[i].StartByte != b[i].StartByte || a[i].EndByte != b[i].EndByte {
			return false
		}
	}
	return true
}

func formatDiags(ds []Diagnostic) string {
	var b strings.Builder
	for _, dg := range ds {
		b.WriteString(dg.Code)
		b.WriteString("@[")
		b.WriteString(jsonInt(dg.StartByte))
		b.WriteString(",")
		b.WriteString(jsonInt(dg.EndByte))
		b.WriteString(") ")
	}
	return b.String()
}

func jsonInt(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

func TestParseJSON(t *testing.T) {
	for _, tc := range cases {
		data, err := json.Marshal(Parse(tc.in))
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		if !utf8.Valid(data) {
			t.Fatalf("%s: JSON is not valid UTF-8", tc.name)
		}
		var back Result
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", tc.name, data, err)
		}
		if !equalTokens(back.Tokens, Parse(tc.in).Tokens) {
			t.Fatalf("%s: JSON round trip changed tokens", tc.name)
		}
	}
}

// TestParseDiagnosticsCoverMarkup checks every documented code is produced by a
// case in the table, so no code path is only exercised by the fuzz corpus.
func TestParseDiagnosticsCoverMarkup(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range cases {
		for _, dg := range Parse(tc.in).Diagnostics {
			seen[dg.Code] = true
		}
	}
	for _, code := range []string{DiagInvalidUTF8, DiagInvalidBlank, DiagUnclosedMath, DiagUnclosedBold, DiagUnclosedItalic, DiagUnclosedUnderline, DiagEmptySpan} {
		if !seen[code] {
			t.Errorf("no table case produces diagnostic %s", code)
		}
	}
}

// roundTrip rebuilds the source from tokens. It only holds for inputs without
// escapes or degradation, and proves valid markup is preserved losslessly.
func roundTrip(tokens []Token) string {
	var b strings.Builder
	for _, tok := range tokens {
		switch tok.Type {
		case TypeText:
			b.WriteString(tok.Text)
		case TypeMath:
			b.WriteString("$" + tok.Text + "$")
		case TypeBold:
			b.WriteString("**" + tok.Text + "**")
		case TypeItalic:
			b.WriteString("*" + tok.Text + "*")
		case TypeUnderline:
			b.WriteString("__" + tok.Text + "__")
		case TypeBlank:
			b.WriteString("{{blank:" + jsonInt(tok.Index) + "}}")
		}
	}
	return b.String()
}

// noRoundTrip lists inputs whose canonical token form differs from the source:
// a style split by math rebuilds as several style spans, and blank numbers lose
// leading zeros.
var noRoundTrip = map[string]bool{
	"math in style trailing":        true,
	"math only in style":            true,
	"math starts italic":            true,
	"math ends underline":           true,
	"math in style middle":          true,
	"two math in style":             true,
	"math protects style delimiter": true,
	"blank leading zeros":           true,
}

func TestParseRoundTrip(t *testing.T) {
	for _, tc := range cases {
		got := Parse(tc.in)
		if len(got.Diagnostics) > 0 || strings.Contains(tc.in, `\`) || noRoundTrip[tc.name] {
			continue
		}
		if back := roundTrip(got.Tokens); back != tc.in {
			t.Errorf("%s: round trip changed the source\n got: %q\nwant: %q", tc.name, back, tc.in)
		}
	}
}

func TestParseBlankWidths(t *testing.T) {
	// A very long digit run is cut at the candidate limit and still diagnosed.
	long := "{{blank:" + strings.Repeat("1", 200) + "}}"
	got := Parse(long)
	if len(got.Tokens) != 1 || got.Tokens[0] != T(long) {
		t.Fatalf("long blank should stay one text token, got %#v", got.Tokens)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != DiagInvalidBlank {
		t.Fatalf("long blank should be diagnosed once, got %v", got.Diagnostics)
	}
}

func FuzzParse(f *testing.F) {
	for _, tc := range cases {
		f.Add(tc.in)
	}
	f.Add("\xff\xfe\xfd")
	f.Add("{{blank:")
	f.Add("**$\\")
	f.Fuzz(func(t *testing.T, in string) {
		res := Parse(in)
		if res.Tokens == nil {
			t.Fatalf("Tokens must never be nil")
		}
		for i, tok := range res.Tokens {
			switch tok.Type {
			case TypeText, TypeMath, TypeBold, TypeItalic, TypeUnderline:
				if tok.Index != 0 {
					t.Fatalf("token %d: index set on %s", i, tok.Type)
				}
				if tok.Text == "" {
					t.Fatalf("token %d: empty %s token", i, tok.Type)
				}
			case TypeBlank:
				if tok.Index < 1 || tok.Text != "" {
					t.Fatalf("token %d: bad blank %#v", i, tok)
				}
			default:
				t.Fatalf("token %d: unknown type %q", i, tok.Type)
			}
			if !utf8.ValidString(tok.Text) {
				t.Fatalf("token %d: text is not valid UTF-8", i)
			}
		}
		for _, dg := range res.Diagnostics {
			if dg.StartByte < 0 || dg.EndByte > len(in) || dg.StartByte > dg.EndByte {
				t.Fatalf("diagnostic %s offsets [%d,%d) outside input of %d bytes", dg.Code, dg.StartByte, dg.EndByte, len(in))
			}
			if dg.Message == "" {
				t.Fatalf("diagnostic %s has empty message", dg.Code)
			}
		}
		if _, err := json.Marshal(res); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	})
}

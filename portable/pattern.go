package portable

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	patternEscapes        = "dDsSwWbBfnrtv"
	patternLiteralEscapes = `\^$.*+?()[]{}|/`
	hexEscapeWidth        = 2
)

// PatternEscape is the meaning of one escape. Class admits it inside a
// character class; Set marks a set of characters, which cannot end a range.
type PatternEscape struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Class      bool   `json:"class"`
	Set        bool   `json:"set"`
}

// PatternSyntax is the portable pattern dialect. VariableWidth lists the
// metacharacters whose match length would depend on the input.
type PatternSyntax struct {
	LiteralEscapes string          `json:"literalEscapes"`
	HexWidth       int             `json:"hexWidth"`
	Escapes        []PatternEscape `json:"escapes"`
	VariableWidth  string          `json:"variableWidth"`
	Dot            string          `json:"dot"`
	End            string          `json:"end"`
}

// The SDK consumes explicit rune sets from the Go compiler, so its engine
// cannot reinterpret whitespace, Unicode atoms or the end-of-text anchor.
func ProfilePatternSyntax() PatternSyntax {
	profile := PatternSyntax{LiteralEscapes: patternLiteralEscapes, HexWidth: hexEscapeWidth, VariableWidth: variableWidthSyntax, Dot: `[^\n]`, End: `(?![\s\S])`}
	for _, name := range patternEscapes {
		node, err := syntax.Parse(`\`+string(name), syntax.Perl)
		if err != nil {
			panic(err)
		}
		escape := PatternEscape{Name: string(name), Class: true}
		switch node.Op {
		case syntax.OpWordBoundary, syntax.OpNoWordBoundary:
			escape.Expression, escape.Class = `\`+string(name), false
		case syntax.OpLiteral:
			escape.Expression = fmt.Sprintf(`[\u{%x}]`, node.Rune[0])
		case syntax.OpCharClass:
			escape.Set = true
			var expression strings.Builder
			expression.WriteByte('[')
			for i := 0; i < len(node.Rune); i += 2 {
				fmt.Fprintf(&expression, `\u{%x}`, node.Rune[i])
				if node.Rune[i] != node.Rune[i+1] {
					fmt.Fprintf(&expression, `-\u{%x}`, node.Rune[i+1])
				}
			}
			expression.WriteByte(']')
			escape.Expression = expression.String()
		default:
			panic("portable: unsupported profile pattern atom")
		}
		profile.Escapes = append(profile.Escapes, escape)
	}
	return profile
}

// Fixed repetitions bound author-controlled backtracking. Admission excludes
// engine-specific syntax before either runtime can attach its own meaning.
func checkPattern(pattern string) error {
	limits := ProfileLimits()
	if len(pattern) > limits.MaxPatternBytes {
		return limit("pattern bytes", limits.MaxPatternBytes)
	}
	scanner := patternScanner{pattern: pattern, maxRepeat: limits.MaxPatternRepeat}
	for scanner.index < len(pattern) {
		if err := scanner.next(); err != nil {
			return err
		}
	}
	_, err := regexp.Compile(pattern)
	return err
}

const variableWidthSyntax = "*+?|()}"

// patternSyntax is the dialect checkPattern admits; ProfilePatternSyntax
// derives it from the Go compiler once.
var patternSyntax = sync.OnceValue(ProfilePatternSyntax)

func (p PatternSyntax) setEscape(name byte) bool {
	return slices.ContainsFunc(p.Escapes, func(escape PatternEscape) bool { return escape.Set && escape.Name == string(name) })
}

// patternScanner walks a pattern once. repeatable records whether the atom
// just scanned consumes input, the only kind of atom a repetition may follow.
type patternScanner struct {
	pattern    string
	index      int
	inClass    bool
	repeatable bool
	maxRepeat  int
}

func (s *patternScanner) next() error {
	char := s.pattern[s.index]
	s.index++
	switch {
	case char == '\\':
		return s.escape()
	case char == '[':
		if s.inClass {
			return errors.New("portable: nested pattern classes are unsupported")
		}
		s.inClass = true
	case char == ']':
		if !s.inClass {
			return errors.New("portable: unmatched pattern class")
		}
		s.inClass = false
		s.repeatable = true
	case s.inClass:
	case strings.ContainsRune(variableWidthSyntax, rune(char)):
		return errors.New("portable: patterns require fixed-width atoms and repetitions")
	case char == '{':
		return s.repetition()
	default:
		s.repeatable = char != '^' && char != '$'
	}
	return nil
}

func (s *patternScanner) escape() error {
	if s.index == len(s.pattern) {
		return errors.New("portable: incomplete pattern escape")
	}
	position, escape := s.index, s.pattern[s.index]
	s.index++
	if s.inClass && patternSyntax().setEscape(escape) && s.rangeEndpoint(position) {
		return errors.New("portable: character class escapes cannot be range endpoints")
	}
	if escape == 'x' {
		return s.hexEscape()
	}
	boundary := escape == 'b' || escape == 'B'
	if s.inClass && boundary || !strings.ContainsRune(patternEscapes+patternLiteralEscapes, rune(escape)) && !(s.inClass && escape == '-') {
		return errors.New("portable: unsupported pattern escape")
	}
	s.repeatable = !boundary
	return nil
}

func (s *patternScanner) rangeEndpoint(position int) bool {
	return position > 1 && s.pattern[position-2] == '-' || position+1 < len(s.pattern) && s.pattern[position+1] == '-'
}

func (s *patternScanner) hexEscape() error {
	if len(s.pattern)-s.index < hexEscapeWidth {
		return errors.New("portable: incomplete hexadecimal escape")
	}
	if _, err := strconv.ParseUint(s.pattern[s.index:s.index+hexEscapeWidth], 16, 8); err != nil {
		return errors.New("portable: invalid hexadecimal escape")
	}
	s.index += hexEscapeWidth
	s.repeatable = true
	return nil
}

// repetition admits only an exact decimal count. RE2 can repeat zero-width
// assertions; Unicode JavaScript cannot, so a count must follow a consuming atom.
func (s *patternScanner) repetition() error {
	if !s.repeatable {
		return errors.New("portable: repetition requires a consuming atom")
	}
	end := strings.IndexByte(s.pattern[s.index:], '}')
	if end < 0 {
		return errors.New("portable: pattern repetition is incomplete")
	}
	raw := s.pattern[s.index : s.index+end]
	count, err := strconv.Atoi(raw)
	if err != nil || len(raw) > 1 && raw[0] == '0' || strings.ContainsFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) || count > s.maxRepeat {
		return errors.New("portable: pattern requires a bounded fixed repetition")
	}
	s.index += end + 1
	s.repeatable = false
	return nil
}

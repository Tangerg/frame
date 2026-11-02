package portable

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
)

const (
	patternEscapes        = "dDsSwWbBfnrtv"
	patternLiteralEscapes = `\^$.*+?()[]{}|/`
	hexEscapeWidth        = 2
)

type PatternEscape struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Class      bool   `json:"class"`
}

type PatternSyntax struct {
	LiteralEscapes string          `json:"literalEscapes"`
	HexWidth       int             `json:"hexWidth"`
	Escapes        []PatternEscape `json:"escapes"`
	Dot            string          `json:"dot"`
	End            string          `json:"end"`
}

// The SDK consumes explicit rune sets from the Go compiler, so its engine
// cannot reinterpret whitespace, Unicode atoms or the end-of-text anchor.
func ProfilePatternSyntax() PatternSyntax {
	profile := PatternSyntax{LiteralEscapes: patternLiteralEscapes, HexWidth: hexEscapeWidth, Dot: `[^\n]`, End: `(?![\s\S])`}
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
	inClass := false
	repeatable := false
	for index := 0; index < len(pattern); index++ {
		char := pattern[index]
		switch char {
		case '\\':
			index++
			if index == len(pattern) {
				return errors.New("portable: incomplete pattern escape")
			}
			escape := pattern[index]
			if inClass && strings.ContainsRune("dDsSwW", rune(escape)) && (index > 1 && pattern[index-2] == '-' || index+1 < len(pattern) && pattern[index+1] == '-') {
				return errors.New("portable: character class escapes cannot be range endpoints")
			}
			if escape == 'x' {
				if len(pattern)-index-1 < hexEscapeWidth {
					return errors.New("portable: incomplete hexadecimal escape")
				}
				if _, err := strconv.ParseUint(pattern[index+1:index+1+hexEscapeWidth], 16, 8); err != nil {
					return errors.New("portable: invalid hexadecimal escape")
				}
				index += hexEscapeWidth
				repeatable = true
				continue
			}
			if inClass && (escape == 'b' || escape == 'B') || !strings.ContainsRune(patternEscapes+patternLiteralEscapes, rune(escape)) && !(inClass && escape == '-') {
				return errors.New("portable: unsupported pattern escape")
			}
			repeatable = escape != 'b' && escape != 'B'
		case '[':
			if inClass {
				return errors.New("portable: nested pattern classes are unsupported")
			}
			inClass = true
		case ']':
			if !inClass {
				return errors.New("portable: unmatched pattern class")
			}
			inClass = false
			repeatable = true
		default:
			if inClass {
				continue
			}
			if strings.ContainsRune("*+?|()}", rune(char)) {
				return errors.New("portable: patterns require fixed-width atoms and repetitions")
			}
			if char == '{' {
				// RE2 can repeat zero-width assertions; Unicode JavaScript cannot.
				// The shared profile admits repetitions only for consuming atoms.
				if !repeatable {
					return errors.New("portable: repetition requires a consuming atom")
				}
				end := strings.IndexByte(pattern[index+1:], '}')
				if end < 0 {
					return errors.New("portable: pattern repetition is incomplete")
				}
				raw := pattern[index+1 : index+1+end]
				count, err := strconv.Atoi(raw)
				if err != nil || len(raw) > 1 && raw[0] == '0' || strings.ContainsFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) || count > limits.MaxPatternRepeat {
					return errors.New("portable: pattern requires a bounded fixed repetition")
				}
				index += end + 1
				repeatable = false
				continue
			}
			repeatable = char != '^' && char != '$'
		}
	}
	_, err := regexp.Compile(pattern)
	return err
}

package value

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// PortableProfileLimits describes the fixed portable value profile, not consumer configuration.
// Transport and application owners continue to choose their payload byte limits.
type PortableProfileLimits struct {
	MaxDescriptorBytes int `json:"maxDescriptorBytes"`
	MaxJSONDepth       int `json:"maxJSONDepth"`
	MaxDescriptorNodes int `json:"maxDescriptorNodes"`
	MaxValueNodes      int `json:"maxValueNodes"`
	MaxReferences      int `json:"maxReferences"`
	MaxReferenceDepth  int `json:"maxReferenceDepth"`
	MaxReferenceWork   int `json:"maxReferenceWork"`
	MaxValidationWork  int `json:"maxValidationWork"`
	MaxEqualityWork    int `json:"maxEqualityWork"`
	MaxNumberBytes     int `json:"maxNumberBytes"`
	MaxAbsExponent     int `json:"maxAbsExponent"`
	MaxPatternBytes    int `json:"maxPatternBytes"`
	MaxPatternRepeat   int `json:"maxPatternRepeat"`

	// Portable runtimes must compare each length and count without rounding.
	MaxSizeConstraint int64 `json:"maxSizeConstraint"`
}

// Returning a value prevents a caller from changing another contract's budget.
func PortableLimits() PortableProfileLimits {
	return PortableProfileLimits{
		MaxDescriptorBytes: 1 << 20,
		MaxJSONDepth:       64,
		MaxDescriptorNodes: 16_384,
		MaxValueNodes:      32 << 20,
		MaxReferences:      1_024,
		MaxReferenceDepth:  64,
		MaxReferenceWork:   16_384,
		MaxValidationWork:  256 << 20,
		MaxEqualityWork:    1 << 20,
		MaxNumberBytes:     1_024,
		MaxAbsExponent:     4_096,
		MaxPatternBytes:    512,
		MaxPatternRepeat:   256,
		MaxSizeConstraint:  1<<53 - 1,
	}
}

var ErrLimit = errors.New("portable: profile limit exceeded")

func NewLimitError(name string, maximum int) error {
	return fmt.Errorf("%w: %s exceeds %d", ErrLimit, name, maximum)
}

type jsonSize struct {
	nodes, depth, keys, equality int
	weight                       uint64
}

func equalityCost(total int, comparisons int, weight uint64) int {
	maximum := PortableLimits().MaxEqualityWork
	if comparisons <= 0 || weight == 0 {
		return total
	}
	if total > maximum || weight > uint64((maximum-total)/comparisons) {
		return maximum + 1
	}
	return total + comparisons*int(weight)
}

// jsonBudget bounds one document before any tree is built. A value has no
// byte budget of its own: its transport owner chooses that limit.
type jsonBudget struct {
	maxBytes int
	maxNodes int
}

func descriptorBudget() jsonBudget {
	limits := PortableLimits()
	return jsonBudget{maxBytes: limits.MaxDescriptorBytes, maxNodes: limits.MaxDescriptorNodes}
}

func valueBudget() jsonBudget {
	return jsonBudget{maxBytes: math.MaxInt, maxNodes: PortableLimits().MaxValueNodes}
}

// checkJSON counts values rather than object member names. It runs before tree
// decoding, numeric expansion, or a recursive schema engine can consume input.
func checkJSON(raw []byte, budget jsonBudget) (jsonSize, error) {
	if len(raw) > budget.maxBytes {
		return jsonSize{}, NewLimitError("descriptor bytes", budget.maxBytes)
	}
	counter := jsonCounter{maxNodes: budget.maxNodes, maxDepth: PortableLimits().MaxJSONDepth}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.ReadToken()
		if err == io.EOF {
			if counter.size.nodes == 0 || len(counter.stack) != 0 {
				return jsonSize{}, errors.New("portable: one complete JSON value is required")
			}
			return counter.size, nil
		}
		if err != nil {
			return jsonSize{}, err
		}
		if err := counter.count(token); err != nil {
			return jsonSize{}, err
		}
	}
}

// jsonFrame is an open object or array. weight is the document weight at its
// start, so its own weight is known when it closes.
type jsonFrame struct {
	kind     jsontext.Kind
	key      bool
	children int
	weight   uint64
}

type jsonCounter struct {
	size     jsonSize
	stack    []jsonFrame
	maxNodes int
	maxDepth int
}

func (c *jsonCounter) count(token jsontext.Token) error {
	kind := token.Kind()
	if kind == '}' || kind == ']' {
		c.closeContainer(kind)
		return nil
	}
	if c.memberName(token) {
		return nil
	}
	if err := c.enterValue(); err != nil {
		return err
	}
	switch kind {
	case '{', '[':
		c.stack = append(c.stack, jsonFrame{kind: kind, key: kind == '{', weight: c.size.weight})
	case '0':
		c.size.weight += uint64(len(token.String()))
		return checkNumber(token.String())
	case '"':
		c.size.weight += uint64(len(token.String()))
	}
	return nil
}

func (c *jsonCounter) closeContainer(kind jsontext.Kind) {
	parent := c.stack[len(c.stack)-1]
	if kind == ']' {
		c.size.equality = equalityCost(c.size.equality, parent.children-1, c.size.weight-parent.weight)
	}
	c.stack = c.stack[:len(c.stack)-1]
}

func (c *jsonCounter) memberName(token jsontext.Token) bool {
	if len(c.stack) == 0 {
		return false
	}
	parent := &c.stack[len(c.stack)-1]
	if parent.kind != '{' || !parent.key {
		return false
	}
	parent.key = false
	c.size.keys++
	c.size.weight += uint64(max(1, len(token.String())))
	return true
}

func (c *jsonCounter) enterValue() error {
	if len(c.stack) > 0 {
		parent := &c.stack[len(c.stack)-1]
		parent.children++
		parent.key = parent.kind == '{'
	} else if c.size.nodes > 0 {
		return errors.New("portable: only one JSON value is permitted")
	}
	c.size.nodes++
	if c.size.nodes > c.maxNodes {
		return NewLimitError("JSON value nodes", c.maxNodes)
	}
	depth := len(c.stack) + 1
	if depth > c.maxDepth {
		return NewLimitError("JSON depth", c.maxDepth)
	}
	c.size.depth = max(c.size.depth, depth)
	c.size.weight++
	return nil
}

func checkNumber(number string) error {
	limits := PortableLimits()
	if len(number) > limits.MaxNumberBytes {
		return NewLimitError("number literal bytes", limits.MaxNumberBytes)
	}
	index := strings.IndexAny(number, "eE")
	if index == -1 {
		return nil
	}
	exponent := strings.TrimLeft(number[index+1:], "+-")
	exponent = strings.TrimLeft(exponent, "0")
	maximum := strconv.Itoa(limits.MaxAbsExponent)
	if len(exponent) > len(maximum) || len(exponent) == len(maximum) && exponent > maximum {
		return NewLimitError("absolute number exponent", limits.MaxAbsExponent)
	}
	return nil
}

// CheckDescriptor also protects constructors that accept an already encoded
// descriptor, before allocating its typed projection.
func CheckDescriptor(raw []byte) error {
	_, err := checkJSON(raw, descriptorBudget())
	return err
}

type descriptorBuffer struct{ bytes.Buffer }

func (b *descriptorBuffer) Write(value []byte) (int, error) {
	if len(value) > PortableLimits().MaxDescriptorBytes-b.Len() {
		return 0, NewLimitError("descriptor bytes", PortableLimits().MaxDescriptorBytes)
	}
	return b.Buffer.Write(value)
}

func CheckDescriptorValue(value any) error {
	var buffer descriptorBuffer
	if err := jsonv2.MarshalWrite(&buffer, value); err != nil {
		return err
	}
	return CheckDescriptor(buffer.Bytes())
}

// decodeBoundedJSON builds the tree of an already counted document; numbers
// keep their lexemes for the schema compiler.
func decodeBoundedJSON(raw []byte) (any, error) {
	var value any
	if err := jsonv2.Unmarshal(raw, &value, Numbers()); err != nil {
		return nil, err
	}
	return value, nil
}

package portable

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Limits describes the fixed portable profile, not consumer configuration.
// Transport and application owners continue to choose their payload byte limits.
type Limits struct {
	MaxDescriptorBytes int `json:"maxDescriptorBytes"`
	MaxJSONDepth       int `json:"maxJSONDepth"`
	MaxDescriptorNodes int `json:"maxDescriptorNodes"`
	MaxValueNodes      int `json:"maxValueNodes"`
	MaxMethods         int `json:"maxMethods"`
	MaxTokenBytes      int `json:"maxTokenBytes"`
	MaxReferences      int `json:"maxReferences"`
	MaxReferenceDepth  int `json:"maxReferenceDepth"`
	MaxReferenceWork   int `json:"maxReferenceWork"`
	MaxValidationWork  int `json:"maxValidationWork"`
	MaxEqualityWork    int `json:"maxEqualityWork"`
	MaxNumberBytes     int `json:"maxNumberBytes"`
	MaxAbsExponent     int `json:"maxAbsExponent"`
	MaxPatternBytes    int `json:"maxPatternBytes"`
	MaxPatternRepeat   int `json:"maxPatternRepeat"`
}

// ProfileLimits is the single source projected to the Node authoring SDK.
// Returning a value prevents a caller from changing another contract's budget.
func ProfileLimits() Limits {
	return Limits{
		MaxDescriptorBytes: 1 << 20,
		MaxJSONDepth:       64,
		MaxDescriptorNodes: 16_384,
		MaxValueNodes:      32 << 20,
		MaxMethods:         256,
		MaxTokenBytes:      256,
		MaxReferences:      1_024,
		MaxReferenceDepth:  64,
		MaxReferenceWork:   16_384,
		MaxValidationWork:  256 << 20,
		MaxEqualityWork:    1 << 20,
		MaxNumberBytes:     1_024,
		MaxAbsExponent:     4_096,
		MaxPatternBytes:    512,
		MaxPatternRepeat:   256,
	}
}

var ErrLimit = errors.New("portable: profile limit exceeded")

func limit(name string, maximum int) error {
	return fmt.Errorf("%w: %s exceeds %d", ErrLimit, name, maximum)
}

type jsonSize struct {
	nodes, depth, keys, equality int
	weight                       uint64
}

func equalityCost(total int, comparisons int, weight uint64) int {
	maximum := ProfileLimits().MaxEqualityWork
	if comparisons <= 0 || weight == 0 {
		return total
	}
	if total > maximum || weight > uint64((maximum-total)/comparisons) {
		return maximum + 1
	}
	return total + comparisons*int(weight)
}

// checkJSON counts values rather than object member names. It runs before tree
// decoding, numeric expansion, or a recursive schema engine can consume input.
func checkJSON(raw []byte, maximumBytes int) (jsonSize, error) {
	limits := ProfileLimits()
	maximumNodes := limits.MaxValueNodes
	if maximumBytes != 0 {
		maximumNodes = limits.MaxDescriptorNodes
	}
	if maximumBytes != 0 && len(raw) > maximumBytes {
		return jsonSize{}, limit("descriptor bytes", maximumBytes)
	}
	type frame struct {
		kind     jsontext.Kind
		key      bool
		children int
		weight   uint64
	}
	var size jsonSize
	var stack []frame
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.ReadToken()
		if err == io.EOF {
			if size.nodes == 0 || len(stack) != 0 {
				return jsonSize{}, errors.New("portable: one complete JSON value is required")
			}
			return size, nil
		}
		if err != nil {
			return jsonSize{}, err
		}
		kind := token.Kind()
		if kind == '}' || kind == ']' {
			parent := stack[len(stack)-1]
			if kind == ']' {
				size.equality = equalityCost(size.equality, parent.children-1, size.weight-parent.weight)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) > 0 {
			parent := &stack[len(stack)-1]
			if parent.kind == '{' && parent.key {
				parent.key = false
				size.keys++
				size.weight += uint64(max(1, len(token.String())))
				continue
			}
			parent.children++
			if parent.kind == '{' {
				parent.key = true
			}
		} else if size.nodes > 0 {
			return jsonSize{}, errors.New("portable: only one JSON value is permitted")
		}
		size.nodes++
		if size.nodes > maximumNodes {
			return jsonSize{}, limit("JSON value nodes", maximumNodes)
		}
		depth := len(stack) + 1
		if depth > limits.MaxJSONDepth {
			return jsonSize{}, limit("JSON depth", limits.MaxJSONDepth)
		}
		size.depth = max(size.depth, depth)
		size.weight++
		if kind == '{' || kind == '[' {
			stack = append(stack, frame{kind: kind, key: kind == '{', weight: size.weight})
		} else if kind == '0' {
			size.weight += uint64(len(token.String()))
			if err := checkNumber(token.String()); err != nil {
				return jsonSize{}, err
			}
		} else if kind == '"' {
			size.weight += uint64(len(token.String()))
		}
	}
}

func checkNumber(number string) error {
	limits := ProfileLimits()
	if len(number) > limits.MaxNumberBytes {
		return limit("number literal bytes", limits.MaxNumberBytes)
	}
	index := strings.IndexAny(number, "eE")
	if index == -1 {
		return nil
	}
	exponent := strings.TrimLeft(number[index+1:], "+-")
	exponent = strings.TrimLeft(exponent, "0")
	maximum := strconv.Itoa(limits.MaxAbsExponent)
	if len(exponent) > len(maximum) || len(exponent) == len(maximum) && exponent > maximum {
		return limit("absolute number exponent", limits.MaxAbsExponent)
	}
	return nil
}

// CheckDescriptor also protects constructors that accept an already encoded
// descriptor, before allocating its typed projection.
func CheckDescriptor(raw []byte) error {
	_, err := checkJSON(raw, ProfileLimits().MaxDescriptorBytes)
	return err
}

type descriptorBuffer struct{ bytes.Buffer }

func (b *descriptorBuffer) Write(value []byte) (int, error) {
	if len(value) > ProfileLimits().MaxDescriptorBytes-b.Len() {
		return 0, limit("descriptor bytes", ProfileLimits().MaxDescriptorBytes)
	}
	return b.Buffer.Write(value)
}

func checkEncodedDescriptor(value any) error {
	var buffer descriptorBuffer
	if err := jsonv2.MarshalWrite(&buffer, value); err != nil {
		return err
	}
	return CheckDescriptor(buffer.Bytes())
}

func decodeBoundedJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

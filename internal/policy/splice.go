package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type objectMember struct {
	key         string
	memberStart int
	valueStart  int
	valueEnd    int
	memberEnd   int
	commaAfter  int
}

type objectScan struct {
	members  []objectMember
	endBrace int
}

// RemoveTopLevelKey removes one top-level object member without re-marshalling
// the surrounding JSON. Nested keys with the same name are untouched.
func RemoveTopLevelKey(raw []byte, key string) ([]byte, error) {
	scan, err := scanTopLevelObject(raw)
	if err != nil {
		return nil, err
	}
	for _, member := range scan.members {
		if member.key != key {
			continue
		}
		start := member.memberStart
		end := member.memberEnd
		if member.commaAfter >= 0 {
			end = member.commaAfter + 1
			for end < len(raw) && isWhitespace(raw[end]) {
				end++
			}
		} else {
			for start > 0 && isWhitespace(raw[start-1]) {
				start--
			}
			if start > 0 && raw[start-1] == ',' {
				start--
			}
		}
		out := make([]byte, 0, len(raw)-(end-start))
		out = append(out, raw[:start]...)
		out = append(out, raw[end:]...)
		return out, nil
	}
	return append([]byte(nil), raw...), nil
}

// ReplaceTopLevelString replaces one top-level string value by splicing in the
// JSON string literal for value. It does not re-marshal the full object.
func ReplaceTopLevelString(raw []byte, key, value string) ([]byte, error) {
	scan, err := scanTopLevelObject(raw)
	if err != nil {
		return nil, err
	}
	for _, member := range scan.members {
		if member.key != key {
			continue
		}
		if member.valueStart >= len(raw) || raw[member.valueStart] != '"' {
			return nil, fmt.Errorf("%q is not a top-level string", key)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(raw)-(member.valueEnd-member.valueStart)+len(encoded))
		out = append(out, raw[:member.valueStart]...)
		out = append(out, encoded...)
		out = append(out, raw[member.valueEnd:]...)
		return out, nil
	}
	return nil, fmt.Errorf("top-level key %q not found", key)
}

// InjectTopLevelObject inserts or replaces a top-level object member with an
// already-encoded JSON object value.
func InjectTopLevelObject(raw []byte, key string, objectValue []byte) ([]byte, error) {
	if !json.Valid(objectValue) {
		return nil, errors.New("injected value is not valid JSON")
	}
	stripped, err := RemoveTopLevelKey(raw, key)
	if err != nil {
		return nil, err
	}
	scan, err := scanTopLevelObject(stripped)
	if err != nil {
		return nil, err
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	addition := make([]byte, 0, len(encodedKey)+1+len(objectValue)+1)
	if len(scan.members) > 0 {
		addition = append(addition, ',')
	}
	addition = append(addition, encodedKey...)
	addition = append(addition, ':')
	addition = append(addition, objectValue...)

	out := make([]byte, 0, len(stripped)+len(addition))
	out = append(out, stripped[:scan.endBrace]...)
	out = append(out, addition...)
	out = append(out, stripped[scan.endBrace:]...)
	return out, nil
}

// topLevelRawValue returns the raw JSON bytes of a top-level member's value and
// whether the key is present. Nested keys with the same name are ignored.
func topLevelRawValue(raw []byte, key string) ([]byte, bool, error) {
	scan, err := scanTopLevelObject(raw)
	if err != nil {
		return nil, false, err
	}
	for _, member := range scan.members {
		if member.key == key {
			return raw[member.valueStart:member.valueEnd], true, nil
		}
	}
	return nil, false, nil
}

func scanTopLevelObject(raw []byte) (objectScan, error) {
	i := skipWhitespace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return objectScan{}, errors.New("JSON value is not an object")
	}
	i++
	var members []objectMember
	seenRoutingKeys := map[string]struct{}{}
	for {
		i = skipWhitespace(raw, i)
		if i >= len(raw) {
			return objectScan{}, errors.New("unexpected end of object")
		}
		if raw[i] == '}' {
			end := i
			i = skipWhitespace(raw, i+1)
			if i != len(raw) {
				return objectScan{}, errors.New("trailing data after object")
			}
			return objectScan{members: members, endBrace: end}, nil
		}
		memberStart := i
		if raw[i] != '"' {
			return objectScan{}, fmt.Errorf("object key at byte %d is not a string", i)
		}
		keyEnd, err := scanString(raw, i)
		if err != nil {
			return objectScan{}, err
		}
		var key string
		if err := json.Unmarshal(raw[i:keyEnd], &key); err != nil {
			return objectScan{}, err
		}
		if key == "model" || key == "provider" {
			if _, ok := seenRoutingKeys[key]; ok {
				return objectScan{}, fmt.Errorf("duplicate top-level key %q", key)
			}
			seenRoutingKeys[key] = struct{}{}
		}
		i = skipWhitespace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return objectScan{}, fmt.Errorf("missing colon after key %q", key)
		}
		valueStart := skipWhitespace(raw, i+1)
		valueEnd, err := scanValue(raw, valueStart)
		if err != nil {
			return objectScan{}, err
		}
		memberEnd := valueEnd
		i = skipWhitespace(raw, valueEnd)
		commaAfter := -1
		if i < len(raw) && raw[i] == ',' {
			commaAfter = i
			i++
		} else if i < len(raw) && raw[i] != '}' {
			return objectScan{}, fmt.Errorf("expected comma or object end at byte %d", i)
		}
		members = append(members, objectMember{
			key:         key,
			memberStart: memberStart,
			valueStart:  valueStart,
			valueEnd:    valueEnd,
			memberEnd:   memberEnd,
			commaAfter:  commaAfter,
		})
	}
}

func scanValue(raw []byte, start int) (int, error) {
	if start >= len(raw) {
		return 0, errors.New("missing JSON value")
	}
	switch raw[start] {
	case '"':
		return scanString(raw, start)
	case '{', '[':
		return scanComposite(raw, start)
	default:
		i := start
		for i < len(raw) && !bytes.ContainsAny(raw[i:i+1], ",}] \t\r\n") {
			i++
		}
		if i == start {
			return 0, fmt.Errorf("invalid JSON value at byte %d", start)
		}
		return i, nil
	}
}

func scanComposite(raw []byte, start int) (int, error) {
	stack := []byte{raw[start]}
	for i := start + 1; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			end, err := scanString(raw, i)
			if err != nil {
				return 0, err
			}
			i = end - 1
		case '{', '[':
			stack = append(stack, raw[i])
		case '}', ']':
			if len(stack) == 0 {
				return 0, fmt.Errorf("unexpected closing delimiter at byte %d", i)
			}
			open := stack[len(stack)-1]
			if (open == '{' && raw[i] != '}') || (open == '[' && raw[i] != ']') {
				return 0, fmt.Errorf("mismatched closing delimiter at byte %d", i)
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, errors.New("unterminated JSON composite")
}

func scanString(raw []byte, start int) (int, error) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, fmt.Errorf("string does not start at byte %d", start)
	}
	escaped := false
	for i := start + 1; i < len(raw); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch raw[i] {
		case '\\':
			escaped = true
		case '"':
			return i + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func skipWhitespace(raw []byte, i int) int {
	for i < len(raw) && isWhitespace(raw[i]) {
		i++
	}
	return i
}

func isWhitespace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}

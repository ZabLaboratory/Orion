package providers

// This file duplicates the LSML canonicalization profile blueruntime
// implements internally (program.go's CanonicalizationProfile —
// "lsml.canonicalise.v1", the generic canonicalizer shared with
// ZabCanvas). blueruntime does not export it (canonicalBytes/digestValue
// are package-private), and Orion needs the identical algorithm to
// compute payload_digest/event_digest on outbound blue.runtime.event.v1
// envelopes the SAME way blueruntime.ParseEvent recomputes and verifies
// them on receipt — any divergence would make every injected event fail
// closed with EVENT_DIGEST_MISMATCH. Values must use json.Number for
// numbers (not int/float64), matching decodeStrict's own convention.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func quoteString(value string) (string, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('"')
	const hex = "0123456789abcdef"
	for len(value) > 0 {
		runeValue, size := utf8.DecodeRuneInString(value)
		if runeValue == utf8.RuneError && size == 1 {
			return "", fmt.Errorf("providers: invalid UTF-8 string")
		}
		value = value[size:]
		switch runeValue {
		case '"', '\\':
			buffer.WriteByte('\\')
			buffer.WriteRune(runeValue)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if runeValue < 0x20 {
				buffer.WriteString(`\u00`)
				buffer.WriteByte(hex[(runeValue>>4)&0xf])
				buffer.WriteByte(hex[runeValue&0xf])
			} else {
				buffer.WriteRune(runeValue)
			}
		}
	}
	buffer.WriteByte('"')
	return buffer.String(), nil
}

func formatNumber(value json.Number) (string, error) {
	raw := string(value)
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return "", fmt.Errorf("providers: invalid canonical number %q", raw)
	}
	if parsed == 0 {
		return "0", nil
	}
	negative := parsed < 0
	if negative {
		parsed = -parsed
	}
	var short string
	if parsed >= 1e-6 && parsed < 1e21 {
		short = strconv.FormatFloat(parsed, 'f', -1, 64)
	} else {
		short = strconv.FormatFloat(parsed, 'e', -1, 64)
		parts := strings.Split(strings.ToLower(short), "e")
		exponent, err := strconv.Atoi(parts[1])
		if err != nil {
			return "", err
		}
		short = parts[0] + "e" + strconv.Itoa(exponent)
		if exponent >= 0 {
			short = parts[0] + "e+" + strconv.Itoa(exponent)
		}
	}
	if negative {
		short = "-" + short
	}
	return short, nil
}

func writeCanonical(buffer *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if value {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		quoted, err := quoteString(value)
		if err != nil {
			return err
		}
		buffer.WriteString(quoted)
	case json.Number:
		number, err := formatNumber(value)
		if err != nil {
			return err
		}
		buffer.WriteString(number)
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			quoted, err := quoteString(key)
			if err != nil {
				return err
			}
			buffer.WriteString(quoted)
			buffer.WriteByte(':')
			if err := writeCanonical(buffer, value[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case []any:
		buffer.WriteByte('[')
		for index, child := range value {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonical(buffer, child); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	default:
		return fmt.Errorf("providers: unsupported canonical value %T (use json.Number for numbers)", value)
	}
	return nil
}

// CanonicalBytes serializes value per the shared LSML canonicalization
// profile: sorted object keys, minimal separators, shortest-round-trip
// number formatting. Mirrors blueruntime's private canonicalBytes exactly.
func CanonicalBytes(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeCanonical(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Digest returns the `sha256:<hex>` digest of value's canonical bytes —
// the same digest domain blueruntime.ParseEvent/ParseProgram verify.
func Digest(value any) (string, error) {
	data, err := CanonicalBytes(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + fmt.Sprintf("%x", sum[:]), nil
}

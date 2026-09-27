package compiler

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// staticNodeFields separates structural node fields from ordinary render
// properties. For scanned objects, props is also the final LayoutNode.Props
// map, so adaptation does not build a second per-node map.
type staticNodeFields struct {
	kind          json.RawMessage
	id            json.RawMessage
	children      json.RawMessage
	animate       json.RawMessage
	bind          json.RawMessage
	bindStyle     json.RawMessage
	bindUniversal json.RawMessage
	bindAnimate   json.RawMessage
	props         map[string]json.RawMessage
}

func (fields *staticNodeFields) set(key string, value json.RawMessage) {
	switch key {
	case "kind":
		fields.kind = value
	case "id":
		fields.id = value
	case "children":
		fields.children = value
	case "animate":
		fields.animate = value
	case "animations":
		// Root animation metadata is read separately from the source layout.
	case "bind":
		fields.bind = value
	case "bindStyle":
		fields.bindStyle = value
	case "bindUniversal":
		fields.bindUniversal = value
	case "bindAnimate":
		fields.bindAnimate = value
	default:
		if fields.props == nil {
			fields.props = make(map[string]json.RawMessage)
		}
		fields.props[key] = value
	}
}

func adaptStaticNode(raw map[string]json.RawMessage, assetBaseURL string, assetsTouched *bool) (LayoutNode, error) {
	var fields staticNodeFields
	for key, value := range raw {
		fields.set(key, value)
	}
	return adaptStaticNodeFields(fields, assetBaseURL, assetsTouched)
}

// adaptStaticNodeJSON adapts ordinary child objects directly from their JSON
// views. The false result requests the compatibility decoder for values or
// syntax this scanner does not handle.
func adaptStaticNodeJSON(raw []byte, assetBaseURL string, assetsTouched *bool) (LayoutNode, bool, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '{' {
		return LayoutNode{}, false, nil
	}
	var fields staticNodeFields
	if !scanStaticNodeFields(data, &fields) {
		return LayoutNode{}, false, nil
	}
	node, err := adaptStaticNodeFields(fields, assetBaseURL, assetsTouched)
	return node, true, err
}

func scanStaticNodeFields(data []byte, fields *staticNodeFields) bool {
	i := skipJSONWhitespace(data, 1)
	if i < len(data) && data[i] == '}' {
		return i == len(data)-1
	}
	for i < len(data) {
		keyStart := i
		keyEnd, ok := scanJSONStringEnd(data, keyStart)
		if !ok {
			return false
		}
		keyRaw := data[keyStart:keyEnd]
		keyBytes := keyRaw[1 : len(keyRaw)-1]
		var key string
		if bytes.IndexByte(keyBytes, '\\') >= 0 || !utf8.Valid(keyBytes) {
			if err := json.Unmarshal(keyRaw, &key); err != nil {
				return false
			}
		} else {
			key = string(keyBytes)
		}
		i = skipJSONWhitespace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return false
		}
		valueStart := skipJSONWhitespace(data, i+1)
		valueEnd, ok := scanJSONValueEnd(data, valueStart)
		if !ok {
			return false
		}
		fields.set(key, json.RawMessage(data[valueStart:valueEnd]))
		i = skipJSONWhitespace(data, valueEnd)
		if i >= len(data) {
			return false
		}
		if data[i] == '}' {
			return i == len(data)-1
		}
		if data[i] != ',' {
			return false
		}
		i = skipJSONWhitespace(data, i+1)
	}
	return false
}

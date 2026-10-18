package lsdpreception

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Normative LSDP tree hash; distinct from LSML scene_version (JCS).
// Number spelling matches ECMAScript's shortest binary64 JSON representation.
func treeHash(value any) string {
	digest := hashNode(value)
	return "tree-sha256:" + hex.EncodeToString(digest)
}
func digest(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}
func size(n int) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, uint64(n)); return b }
func number(v float64) string {
	if v == 0 {
		return "0"
	}
	if math.Abs(v) >= 1e-6 && math.Abs(v) < 1e21 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	s := strconv.FormatFloat(v, 'e', -1, 64)
	pieces := strings.Split(s, "e")
	n, _ := strconv.Atoi(pieces[1])
	sign := ""
	if n >= 0 {
		sign = "+"
	}
	return pieces[0] + "e" + sign + strconv.Itoa(n)
}
func hashNode(value any) []byte {
	switch v := value.(type) {
	case nil:
		return digest([]byte("n"))
	case bool:
		if v {
			return digest([]byte("t"))
		}
		return digest([]byte("f"))
	case float64:
		return digest([]byte("d" + number(v)))
	case string:
		return digest([]byte("s"), size(len([]byte(v))), []byte(v))
	case []any:
		parts := [][]byte{[]byte("a"), size(len(v))}
		for _, item := range v {
			parts = append(parts, hashNode(item))
		}
		return digest(parts...)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var walk func([]string) []byte
		walk = func(ks []string) []byte {
			if len(ks) == 0 {
				return digest([]byte("LSDP-MAP-EMPTY/1"))
			}
			root := 0
			priority := sha256.Sum256([]byte(ks[0]))
			for i := 1; i < len(ks); i++ {
				candidate := sha256.Sum256([]byte(ks[i]))
				cmp := bytes.Compare(candidate[:], priority[:])
				if cmp > 0 || (cmp == 0 && ks[i] > ks[root]) {
					root = i
					priority = candidate
				}
			}
			key := ks[root]
			return digest([]byte("LSDP-MAP-NODE/1"), walk(ks[:root]), size(len([]byte(key))), []byte(key), hashNode(v[key]), walk(ks[root+1:]))
		}
		return digest([]byte("o"), size(len(keys)), walk(keys))
	default:
		panic("non-JSON native state")
	}
}

// Apply on an owned clone so any failed operation rolls back the full batch.
func applyObjectOperations(state any, ops []map[string]any) (any, error) {
	raw, err := json.Marshal(map[string]any{"state": state, "ops": ops})
	if err != nil {
		return nil, err
	}
	var owned struct {
		State any              `json:"state"`
		Ops   []map[string]any `json:"ops"`
	}
	if err = json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	next := owned.State
	for _, operation := range owned.Ops {
		path, ok := operation["path"].(string)
		if !ok || !strings.HasPrefix(path, "/") {
			return nil, errors.New("NATIVE_OBJECT_POINTER_REQUIRED")
		}
		parts := strings.Split(path[1:], "/")
		for i, part := range parts {
			for k := 0; k < len(part); k++ {
				if part[k] == '~' {
					if k+1 >= len(part) || (part[k+1] != '0' && part[k+1] != '1') {
						return nil, errors.New("NATIVE_PATCH_POINTER_ESCAPE")
					}
					k++
				}
			}
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		}
		next, err = patchNode(next, parts, operation)
		if err != nil {
			return nil, err
		}
	}
	return next, nil
}
func pointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

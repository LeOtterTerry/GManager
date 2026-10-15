package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

// Doc is a JSON object that keeps key order and the raw encoding of each value.
// Unknown keys can be copied through a save without being reinterpreted.
type Doc struct {
	Order  []string
	Values map[string]json.RawMessage
}

// Parse decodes a JSON object. Duplicate keys and trailing data are errors.
func Parse(b []byte) (*Doc, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return &Doc{Values: map[string]json.RawMessage{}}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("config must be a JSON object: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("config must be a JSON object")
	}
	doc := &Doc{Values: map[string]json.RawMessage{}}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("config key must be a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, exists := doc.Values[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		doc.Order = append(doc.Order, key)
		doc.Values[key] = bytes.TrimSpace(raw)
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok = tok.(json.Delim); !ok || delim != '}' {
		return nil, errors.New("config must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing data after JSON object")
		}
		return nil, fmt.Errorf("trailing data after JSON object: %w", err)
	}
	return doc, nil
}

// Marshal writes the object with stable indentation. Each value is copied
// from its raw JSON so unknown keys are not reformatted.
func (d *Doc) Marshal() []byte {
	if d == nil || len(d.Order) == 0 {
		return []byte("{}\n")
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range d.Order {
		enc, err := json.Marshal(key)
		if err != nil {
			enc = []byte(`""`)
		}
		buf.WriteString("  ")
		buf.Write(enc)
		buf.WriteString(": ")
		buf.Write(bytes.TrimSpace(d.Values[key]))
		if i != len(d.Order)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes()
}

func (d *Doc) get(key string) (json.RawMessage, bool) {
	if d == nil {
		return nil, false
	}
	raw, ok := d.Values[key]
	return raw, ok
}

func rawEqual(a json.RawMessage, aOK bool, b json.RawMessage, bOK bool) bool {
	if aOK != bOK {
		return false
	}
	if !aOK {
		return true
	}
	return jsonEqual(a, b)
}

func jsonEqual(a, b json.RawMessage) bool {
	var ja, jb any
	if err := json.Unmarshal(a, &ja); err != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		return false
	}
	return reflect.DeepEqual(ja, jb)
}

func docsSameRaw(a, b *Doc) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if len(a.Order) != len(b.Order) {
		return false
	}
	for i, key := range a.Order {
		if b.Order[i] != key {
			return false
		}
		if !bytes.Equal(bytes.TrimSpace(a.Values[key]), bytes.TrimSpace(b.Values[key])) {
			return false
		}
	}
	return true
}

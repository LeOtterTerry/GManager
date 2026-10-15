package plainsofpain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

// worldSizes are the verified server.json worldSize values.
var worldSizes = map[int64]struct{}{
	11: {},
	21: {},
	31: {},
	41: {},
	51: {},
}

// difficulties are the verified values: 0–4, and 255 for custom.
var difficulties = map[int64]struct{}{
	0: {}, 1: {}, 2: {}, 3: {}, 4: {}, 255: {},
}

// Validate checks the server.json rules that are already verified.
// worldId, mapId, seed, ttr, and adminAccountIDs are preserved and not
// type-checked: their formats are unverified.
func (m *Module) Validate(cfg []byte) []core.FieldError {
	trimmed := bytes.TrimSpace(cfg)
	if len(trimmed) == 0 {
		return []core.FieldError{{Field: "", Message: "config is empty"}}
	}
	if trimmed[0] != '{' {
		return []core.FieldError{{Field: "", Message: "config must be a JSON object"}}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return []core.FieldError{{Field: "", Message: "config must be a JSON object"}}
	}
	var errs []core.FieldError
	if raw, ok := obj["serverName"]; ok {
		if _, ok := asString(raw); !ok {
			errs = append(errs, core.FieldError{Field: "serverName", Message: "must be a string"})
		}
	}
	if raw, ok := obj["password"]; ok {
		s, ok := asString(raw)
		if !ok {
			errs = append(errs, core.FieldError{Field: "password", Message: "must be a string"})
		} else if utf8.RuneCountInString(s) > PasswordMaxRunes {
			errs = append(errs, core.FieldError{Field: "password", Message: fmt.Sprintf("must be %d characters or fewer", PasswordMaxRunes)})
		}
	}
	if raw, ok := obj["maxPlayers"]; ok {
		n, ok := asInt(raw)
		if !ok || n < MaxPlayersMin || n > MaxPlayersMax {
			errs = append(errs, core.FieldError{Field: "maxPlayers", Message: fmt.Sprintf("must be an integer from %d to %d", MaxPlayersMin, MaxPlayersMax)})
		}
	}
	if raw, ok := obj["difficulty"]; ok {
		n, ok := asInt(raw)
		if _, allowed := difficulties[n]; !ok || !allowed {
			errs = append(errs, core.FieldError{Field: "difficulty", Message: "must be an integer 0, 1, 2, 3, 4, or 255"})
		}
	}
	if raw, ok := obj["worldSize"]; ok {
		n, ok := asInt(raw)
		if _, allowed := worldSizes[n]; !ok || !allowed {
			errs = append(errs, core.FieldError{Field: "worldSize", Message: "must be an integer 11, 21, 31, 41, or 51"})
		}
	}
	if raw, ok := obj["port"]; ok {
		if _, ok := asPort(raw); !ok {
			errs = append(errs, core.FieldError{Field: "port", Message: "must be an integer from 1 to 65535"})
		}
	}
	if raw, ok := obj["queryPort"]; ok {
		if _, ok := asPort(raw); !ok {
			errs = append(errs, core.FieldError{Field: "queryPort", Message: "must be an integer from 1 to 65535"})
		}
	}
	if raw, ok := obj["userdir"]; ok {
		if _, ok := asString(raw); !ok {
			errs = append(errs, core.FieldError{Field: "userdir", Message: "must be a string"})
		}
	}
	return errs
}

// Warnings reports non-default ports. A custom-port bug has been reported,
// so v0.1 keeps the defaults unless the operator accepts the warning.
func (m *Module) Warnings(cfg []byte) []string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return nil
	}
	var out []string
	if raw, ok := obj["port"]; ok {
		if port, ok := asInt(raw); ok && port != DefaultGamePort {
			out = append(out, fmt.Sprintf("port %d is not the default %d; a custom-port bug has been reported for Plains of Pain", port, DefaultGamePort))
		}
	}
	if raw, ok := obj["queryPort"]; ok {
		if port, ok := asInt(raw); ok && port != DefaultQueryPort {
			out = append(out, fmt.Sprintf("queryPort %d is not the default %d; a custom-port bug has been reported for Plains of Pain", port, DefaultQueryPort))
		}
	}
	return out
}

// Ports reads UDP port and queryPort, falling back to the verified defaults
// when a key is absent. Invalid values also fall back; Validate reports them.
func (m *Module) Ports(cfg []byte) []core.PortSpec {
	game, query := DefaultGamePort, DefaultQueryPort
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err == nil {
		if raw, ok := obj["port"]; ok {
			if port, ok := asPort(raw); ok {
				game = port
			}
		}
		if raw, ok := obj["queryPort"]; ok {
			if port, ok := asPort(raw); ok {
				query = port
			}
		}
	}
	return []core.PortSpec{
		{Name: "game", Protocol: "udp", Port: game},
		{Name: "query", Protocol: "udp", Port: query},
	}
}

// UserDir returns userdir when it is a non-empty string.
// The format of other save settings is not inferred from this.
func UserDir(cfg []byte) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return "", false
	}
	raw, ok := obj["userdir"]
	if !ok {
		return "", false
	}
	s, ok := asString(raw)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func asInt(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		// A quoted numeral is a string. json.Number would accept it; the
		// verified fields are JSON numbers, not numeric strings.
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

func asPort(raw json.RawMessage) (int, bool) {
	n, ok := asInt(raw)
	if !ok || n < 1 || n > 65535 {
		return 0, false
	}
	return int(n), true
}

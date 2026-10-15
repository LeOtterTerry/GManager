package plainsofpain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAcceptsBoundaries(t *testing.T) {
	mod := New()
	ok := []string{
		`{"password":"","maxPlayers":1,"difficulty":0,"worldSize":11}`,
		`{"password":"12345678901234567890","maxPlayers":200,"difficulty":4,"worldSize":51}`,
		`{"password":"éééééééééééééééééééé","difficulty":255,"worldSize":31}`,
		`{"serverName":"Plains","port":7777,"queryPort":27016}`,
		`{"serverName":"Åland","port":65535,"queryPort":27016}`,
		`{"futureKey":{"n":1},"ttr":["unverified"],"worldId":1,"mapId":"x","seed":{"n":9}}`,
		`{"adminAccountIDs":["STEAM64_PLACEHOLDER"]}`,
		`{"adminAccountIDs":{"todo":"format unverified"}}`,
		`{"adminAccountIDs":"STEAM64_PLACEHOLDER"}`,
	}
	for _, body := range ok {
		if errs := mod.Validate([]byte(body)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %+v", body, errs)
		}
	}
	if n := len([]rune("éééééééééééééééééééé")); n != 20 {
		t.Fatalf("rune count = %d", n)
	}
}

func TestValidateRejectsKnownRules(t *testing.T) {
	mod := New()
	cases := []struct {
		body  string
		field string
	}{
		{`{"password":"123456789012345678901"}`, "password"},
		{`{"password":"ééééééééééééééééééééx"}`, "password"},
		{`{"password":12}`, "password"},
		{`{"maxPlayers":0}`, "maxPlayers"},
		{`{"maxPlayers":201}`, "maxPlayers"},
		{`{"maxPlayers":1.5}`, "maxPlayers"},
		{`{"maxPlayers":"10"}`, "maxPlayers"},
		{`{"difficulty":-1}`, "difficulty"},
		{`{"difficulty":5}`, "difficulty"},
		{`{"difficulty":254}`, "difficulty"},
		{`{"difficulty":255.0}`, "difficulty"},
		{`{"worldSize":10}`, "worldSize"},
		{`{"worldSize":12}`, "worldSize"},
		{`{"worldSize":20}`, "worldSize"},
		{`{"serverName":1}`, "serverName"},
		{`{"serverName":""}`, "serverName"},
		{"{\"serverName\":\"bad\\nname\"}", "serverName"},
		{`{"serverName":"` + strings.Repeat("a", 65) + `"}`, "serverName"},
		{`{"userdir":1}`, "userdir"},
		{`{"port":0}`, "port"},
		{`{"port":65536}`, "port"},
		{`{"port":70000}`, "port"},
		{`{"queryPort":0}`, "queryPort"},
		{`{"queryPort":65536}`, "queryPort"},
		{`{"queryPort":"27016"}`, "queryPort"},
		{`{"port":27016}`, "queryPort"},
		{`{"port":7777,"queryPort":7777}`, "queryPort"},
		{`{"port":8787}`, "port"},
		{`{"queryPort":5173}`, "queryPort"},
		{`[]`, ""},
		{`null`, ""},
		{``, ""},
	}
	for _, tc := range cases {
		errs := mod.Validate([]byte(tc.body))
		if len(errs) == 0 {
			t.Errorf("Validate(%s) accepted an invalid config", tc.body)
			continue
		}
		if errs[0].Field != tc.field {
			t.Errorf("Validate(%s) field = %q, want %q (%s)", tc.body, errs[0].Field, tc.field, errs[0].Message)
		}
	}
}

func TestWarningsOnNonDefaultPortsOnly(t *testing.T) {
	mod := New()
	if w := mod.Warnings([]byte(`{"port":7777,"queryPort":27016}`)); len(w) != 0 {
		t.Fatalf("default warnings = %v", w)
	}
	if w := mod.Warnings([]byte(`{}`)); len(w) != 0 {
		t.Fatalf("absent port warnings = %v", w)
	}
	w := mod.Warnings([]byte(`{"port":7778,"queryPort":27017}`))
	if len(w) != 2 {
		t.Fatalf("warnings = %v", w)
	}
	if !strings.Contains(w[0], "7778") || !strings.Contains(w[1], "27017") {
		t.Fatalf("warnings = %v", w)
	}
	if errs := mod.Validate([]byte(`{"port":7778,"queryPort":27017}`)); len(errs) != 0 {
		t.Fatalf("non-default ports should warn, not fail: %+v", errs)
	}
}

func TestSchemaMatchesVerifiedRules(t *testing.T) {
	var schema struct {
		Additional bool `json:"additionalProperties"`
		Properties map[string]struct {
			MaxLength *int  `json:"maxLength"`
			Minimum   *int  `json:"minimum"`
			Maximum   *int  `json:"maximum"`
			Enum      []int `json:"enum"`
			Default   *int  `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(New().Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if !schema.Additional {
		t.Fatal("schema must allow unknown keys")
	}
	for _, key := range []string{
		"serverName", "password", "maxPlayers", "worldId", "mapId", "seed",
		"difficulty", "worldSize", "port", "queryPort", "ttr", "userdir", "adminAccountIDs",
	} {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("schema missing %s", key)
		}
	}
	pw := schema.Properties["password"]
	if pw.MaxLength == nil || *pw.MaxLength != PasswordMaxRunes {
		t.Fatalf("password maxLength = %v", pw.MaxLength)
	}
	name := schema.Properties["serverName"]
	if name.MaxLength == nil || *name.MaxLength != ServerNameMaxRunes {
		t.Fatalf("serverName maxLength = %v", name.MaxLength)
	}
	players := schema.Properties["maxPlayers"]
	if players.Minimum == nil || players.Maximum == nil || *players.Minimum != MaxPlayersMin || *players.Maximum != MaxPlayersMax {
		t.Fatalf("maxPlayers = %+v", players)
	}
	if !sameInts(schema.Properties["difficulty"].Enum, []int{0, 1, 2, 3, 4, 255}) {
		t.Fatalf("difficulty enum = %v", schema.Properties["difficulty"].Enum)
	}
	if !sameInts(schema.Properties["worldSize"].Enum, []int{11, 21, 31, 41, 51}) {
		t.Fatalf("worldSize enum = %v", schema.Properties["worldSize"].Enum)
	}
	if schema.Properties["port"].Default == nil || *schema.Properties["port"].Default != DefaultGamePort {
		t.Fatalf("port default = %v", schema.Properties["port"].Default)
	}
	if schema.Properties["queryPort"].Default == nil || *schema.Properties["queryPort"].Default != DefaultQueryPort {
		t.Fatalf("queryPort default = %v", schema.Properties["queryPort"].Default)
	}
	// Unverified fields stay untyped in the schema.
	raw := map[string]json.RawMessage{}
	props := map[string]json.RawMessage{}
	if err := json.Unmarshal(New().Schema(), &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw["properties"], &props); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"worldId", "mapId", "seed", "ttr", "adminAccountIDs"} {
		if strings.Contains(string(props[key]), `"type"`) {
			t.Errorf("%s schema guesses a type: %s", key, props[key])
		}
	}
}

func sameInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

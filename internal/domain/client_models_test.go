package domain

import "testing"

func TestClientModelsValidate(t *testing.T) {
	base := func() ClientModels {
		return ClientModels{Version: 1, Enabled: true, DefaultModel: "a", Models: []ClientModel{{ID: "a", ReasoningLevels: []string{"low", "high"}, DefaultReasoning: "high"}}}
	}
	for _, tc := range []struct {
		name   string
		change func(*ClientModels)
		valid  bool
	}{
		{"valid", func(*ClientModels) {}, true},
		{"missing default", func(c *ClientModels) { c.DefaultModel = "gone" }, false},
		{"unsupported default", func(c *ClientModels) { c.Models[0].DefaultReasoning = "ultra" }, false},
		{"unknown level", func(c *ClientModels) { c.Models[0].ReasoningLevels = []string{"made-up"} }, false},
		{"duplicate levels", func(c *ClientModels) { c.Models[0].ReasoningLevels = []string{"high", "high"} }, false},
		{"empty publication", func(c *ClientModels) { c.Models = nil }, false},
		{"case collision", func(c *ClientModels) { c.Models = append(c.Models, ClientModel{ID: "A"}) }, false},
		{"unsafe id", func(c *ClientModels) { c.Models[0].ID = "a\nmodel_provider=x" }, false},
		{"omit parameters", func(c *ClientModels) { c.Models[0].ReasoningLevels = nil; c.Models[0].DefaultReasoning = "" }, true},
		{"none is an explicit level", func(c *ClientModels) {
			c.Models[0].ReasoningLevels = []string{"none"}
			c.Models[0].DefaultReasoning = "none"
		}, true},
		{"omit must not have default", func(c *ClientModels) { c.Models[0].ReasoningLevels = nil }, false},
		{"empty disabled draft", func(c *ClientModels) { c.Enabled = false; c.DefaultModel = ""; c.Models = nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.change(&c)
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestClientModelsV2OnlyValidatesDefault(t *testing.T) {
	c := ClientModels{Version: ClientModelsVersion, Enabled: true, DefaultModel: "a", Models: []ClientModel{{ID: "a", DefaultReasoning: "xhigh"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Models[0].DefaultReasoning = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Models[0].DefaultReasoning = "unrecognized"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted invalid default")
	}
}

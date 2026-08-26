package main

import "testing"

func TestValidateCommandProtectsDownMigrations(t *testing.T) {
	for _, test := range []struct {
		command   string
		arguments []string
		optIn     string
		valid     bool
	}{
		{command: "up", valid: true},
		{command: "status", valid: true},
		{command: "down-to", arguments: []string{"20"}, optIn: "1", valid: true},
		{command: "down-to", arguments: []string{"20"}, valid: false},
		{command: "down-to", arguments: []string{"0"}, optIn: "1", valid: false},
		{command: "down", optIn: "1", valid: false},
	} {
		err := validateCommand(test.command, test.arguments, test.optIn)
		if (err == nil) != test.valid {
			t.Errorf("validateCommand(%q, %v, %q) error = %v, valid=%v", test.command, test.arguments, test.optIn, err, test.valid)
		}
	}
}

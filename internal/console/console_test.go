package console

import "testing"

func TestDebug(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"no flag, no env", nil, "", false},
		{"flag alone", []string{"--debug"}, "", true},
		{"flag among other args", []string{"--other", "--debug", "x"}, "", true},
		{"flag lookalike ignored", []string{"--debugger", "debug", "-debug"}, "", false},
		{"env 1", nil, "1", true},
		{"env true (any case)", nil, "TRUE", true},
		{"env yes", nil, "yes", true},
		{"env on", nil, "on", true},
		{"env with whitespace", nil, " 1 ", true},
		{"env 0", nil, "0", false},
		{"env garbage", nil, "sure", false},
		{"env empty", nil, "", false},
		{"flag wins even with env 0", []string{"--debug"}, "0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(string) string { return tt.env }
			if got := Debug(tt.args, getenv); got != tt.want {
				t.Errorf("Debug(%v, env=%q) = %v, want %v", tt.args, tt.env, got, tt.want)
			}
		})
	}
}

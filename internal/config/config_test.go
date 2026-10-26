package config

import "testing"

func TestParseBasePath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"/", "", false},
		{"/schedule", "/schedule", false},
		{"schedule", "/schedule", false},
		{"/schedule/", "/schedule", false},
		{" /homerun/schedule ", "/homerun/schedule", false},
		{"/a//b", "", true},
		{"/../schedule", "", true},
		{"/schedule\"><script>", "", true},
		{"/schedule?x=1", "", true},
	}
	for _, tc := range cases {
		got, err := ParseBasePath(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseBasePath(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBasePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseBool(t *testing.T) {
	for in, want := range map[string]bool{"": false, "false": false, "0": false, "yes": false, "true": true, "TRUE": true, " 1 ": true} {
		if got := ParseBool("X", in); got != want {
			t.Errorf("ParseBool(%q) = %v, want %v", in, got, want)
		}
	}
}

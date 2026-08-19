package schema

import (
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare object", in: `{"a":1}`, want: `{"a":1}`},
		{name: "bare object with surrounding whitespace", in: "  \n{\"a\":1}\n  ", want: `{"a":1}`},
		{name: "bare array", in: `[1,2,3]`, want: `[1,2,3]`},
		{
			name: "fenced with language tag",
			in:   "```json\n{\"a\":1}\n```",
			want: `{"a":1}`,
		},
		{
			name: "fenced without language tag",
			in:   "```\n{\"a\":1}\n```",
			want: `{"a":1}`,
		},
		{
			name: "prose before and after a JSON object",
			in:   "Sure, here's the result:\n\n{\"a\":1}\n\nLet me know if that works!",
			want: `{"a":1}`,
		},
		{name: "prose only, no JSON", in: "I can't do that right now.", wantErr: true},
		{name: "empty string", in: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractJSON(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractJSON(%q): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("ExtractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

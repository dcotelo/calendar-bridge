package googleauth

import "testing"

func TestExtractAuthCode(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "bare code",
			input: "4/0AVGzR1abcXYZ123\n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "bare code no trailing newline",
			input: "4/0AVGzR1abcXYZ123",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "full redirect URL",
			input: "http://localhost:1/?code=4%2F0AVGzR1abcXYZ123&scope=https://www.googleapis.com/auth/calendar.events\n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "https redirect URL",
			input: "https://localhost/?state=state-token&code=abc123&scope=foo\n",
			want:  "abc123",
		},
		{
			name:  "surrounding whitespace",
			input: "   4/0AVGzR1abcXYZ123   \n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:    "empty input",
			input:   "\n",
			wantErr: true,
		},
		{
			name:    "URL missing code param",
			input:   "http://localhost:1/?scope=foo\n",
			wantErr: true,
		},
		{
			name:    "malformed URL",
			input:   "http://[::1\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractAuthCode(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("extractAuthCode(%q) error = nil, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractAuthCode(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("extractAuthCode(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

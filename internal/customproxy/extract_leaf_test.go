package customproxy

import "testing"

func TestExtractLeaf(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "provider prefix and v1",
			input: "/api/provider/openai/v1/chat/completions",
			want:  "/chat/completions",
		},
		{
			name:  "anthropic messages",
			input: "/api/provider/anthropic/v1/messages",
			want:  "/messages",
		},
		{
			name:  "google v1beta with action suffix",
			input: "/api/provider/google/v1beta/models/gpt-5.4:generateContent",
			want:  "/models/gpt-5.4:generateContent",
		},
		{
			name:  "google v1beta1",
			input: "/api/provider/google/v1beta1/models/x",
			want:  "/models/x",
		},
		{
			name:  "bare v1 prefix",
			input: "/v1/chat/completions",
			want:  "/chat/completions",
		},
		{
			name:  "already leaf",
			input: "/chat/completions",
			want:  "/chat/completions",
		},
		{
			name:  "provider prefix with no trailing segment",
			input: "/api/provider/foo",
			want:  "/",
		},
		{
			name:  "bare v1beta prefix",
			input: "/v1beta/models",
			want:  "/models",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := extractLeaf(tc.input)
			if got != tc.want {
				t.Errorf("extractLeaf(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

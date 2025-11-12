package utils

import "testing"

func TestForLog(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Plain ASCII string",
			input:    "Hello, World! This is a test.",
			expected: "Hello, World! This is a test.",
		},
		{
			name:     "String with null bytes",
			input:    "Hello\x00World",
			expected: "HelloWorld",
		},
		{
			name:     "String with control characters (tab and newline)",
			input:    "Line1\tLine2\nLine3",
			expected: "Line1Line2Line3",
		},
		{
			name:     "String with printable Unicode",
			input:    "你好, 世界",
			expected: "你好, 世界",
		},
		{
			name:     "Empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ForLog(tt.input); got != tt.expected {
				t.Errorf("ForLog() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestForHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "String with HTML special characters",
			input:    `<script>alert("XSS & Injection")</script>`,
			expected: `&lt;script&gt;alert(&#34;XSS &amp; Injection&#34;)&lt;/script&gt;`,
		},
		{
			name:     "String with both control characters and HTML",
			input:    "<\x00script>alert('XSS')\n</script>",
			expected: `&lt;script&gt;alert(&#39;XSS&#39;)&lt;/script&gt;`,
		},
		{
			name:     "Plain string",
			input:    "This is a safe string.",
			expected: "This is a safe string.",
		},
		{
			name:     "Empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ForHTML(tt.input); got != tt.expected {
				t.Errorf("ForHTML() = %q, want %q", got, tt.expected)
			}
		})
	}
}

package toolkit

import "testing"

func TestFailedJSONReadsMetadataWithoutInspectingBody(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{`{"text":"failed","is_error":true}`, true},
		{"{\"complete\":false,\"error\":\"unexpected EOF\"}\n\n<html>partial</html>", true},
		{"{\"complete\":true}\n\n{\"error\":\"server body, not a tool error\"}", false},
		{"{\"status_code\":500}\n\nError: server response", false},
		{"Error: request denied", true},
	} {
		if got := FailedJSON(tc.text); got != tc.want {
			t.Errorf("FailedJSON(%q)=%v want %v", tc.text, got, tc.want)
		}
	}
}

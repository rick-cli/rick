package catalog

import (
	"encoding/base64"
	"testing"
)

func mkJWT(payload string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func TestAccountIDExtraction(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"top-level", `{"chatgpt_account_id":"acc-1"}`, "acc-1"},
		{"nested", `{"https://api.openai.com/auth":{"chatgpt_account_id":"acc-2"}}`, "acc-2"},
		{"organizations", `{"organizations":[{"id":"org-1"}]}`, "org-1"},
		{"both-prefers-top", `{"chatgpt_account_id":"acc-top","https://api.openai.com/auth":{"chatgpt_account_id":"acc-nested"}}`, "acc-top"},
		{"garbage", `not-json`, ""},
	}
	for _, c := range cases {
		if got := accountIDFromJWT(mkJWT(c.payload)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

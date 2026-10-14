package push

import "testing"

func TestUnregistered(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{404, `{}`, true},
		{400, `{"error":{"status":"INVALID_ARGUMENT","details":[{"errorCode":"UNREGISTERED"}]}}`, true},
		{400, `{"error":{"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`, true},
		{400, `{"error":{"status":"INVALID_ARGUMENT","message":"Invalid JSON payload"}}`, false},
		{503, `{"error":{"status":"UNAVAILABLE"}}`, false},
	}
	for _, c := range cases {
		if got := unregistered(c.status, []byte(c.body)); got != c.want {
			t.Errorf("unregistered(%d, %s) = %v, attendu %v", c.status, c.body, got, c.want)
		}
	}
}

package feishu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendFetchesTokenThenMessagesRecipient(t *testing.T) {
	var auth string
	var msg map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			var creds map[string]string
			json.NewDecoder(r.Body).Decode(&creds)
			if creds["app_id"] != "cli_a" || creds["app_secret"] != "secret" {
				t.Errorf("credentials: %v", creds)
			}
			w.Write([]byte(`{"code":0,"tenant_access_token":"tok"}`))
		case "/open-apis/im/v1/messages":
			if got := r.URL.Query().Get("receive_id_type"); got != "open_id" {
				t.Errorf("receive_id_type = %q", got)
			}
			auth = r.Header.Get("Authorization")
			json.NewDecoder(r.Body).Decode(&msg)
			w.Write([]byte(`{"code":0}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	s := &Sender{BaseURL: srv.URL, AppID: "cli_a", AppSecret: "secret", OpenID: "ou_1"}
	if err := s.Send("Pawprint · Purchase", "Today: $9", "<b>ignored</b>"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || msg["receive_id"] != "ou_1" || msg["msg_type"] != "text" {
		t.Errorf("auth %q, message %v", auth, msg)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(msg["content"]), &content); err != nil {
		t.Fatal(err)
	}
	if content["text"] != "Pawprint · Purchase\nToday: $9" {
		t.Errorf("text = %q", content["text"])
	}
}

// Feishu reports failures as a non-zero code, sometimes with HTTP 200; the handler relies
// on the error to make Apple retry the notification.
func TestSendReturnsAPIErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		token, reply string
		want         string
	}{
		"token rejected": {http.StatusBadRequest, `{"code":10003,"msg":"invalid param"}`, "", "invalid param"},
		"message failed": {http.StatusOK, `{"code":0,"tenant_access_token":"tok"}`, `{"code":230013,"msg":"out of range"}`, "out of range"},
		"not JSON":       {http.StatusBadGateway, `bad gateway`, "", "HTTP 502"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/internal") {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.token))
					return
				}
				w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			err := (&Sender{BaseURL: srv.URL}).Send("s", "", "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Package feishu sends text messages to one person through a Feishu or Lark app bot.
package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Sender implements mailer.Sender. A token is fetched for every message: messages are
// rare, and this leaves no token state to refresh.
type Sender struct {
	BaseURL   string // https://open.feishu.cn, or https://open.larksuite.com for Lark
	AppID     string
	AppSecret string
	OpenID    string // recipient; an open_id is only valid for the app that issued it
}

var client = &http.Client{Timeout: 10 * time.Second}

// Send delivers the subject and text as one message. The HTML part is not used.
func (s *Sender) Send(subject, textBody, _ string) error {
	var auth struct {
		Token string `json:"tenant_access_token"`
	}
	if err := s.post("/open-apis/auth/v3/tenant_access_token/internal", "", map[string]string{
		"app_id": s.AppID, "app_secret": s.AppSecret,
	}, &auth); err != nil {
		return fmt.Errorf("feishu token: %w", err)
	}

	text := subject
	if textBody != "" {
		text += "\n" + textBody
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	if err := s.post("/open-apis/im/v1/messages?receive_id_type=open_id", auth.Token, map[string]string{
		"receive_id": s.OpenID, "msg_type": "text", "content": string(content),
	}, nil); err != nil {
		return fmt.Errorf("feishu message: %w", err)
	}
	return nil
}

// post sends a JSON request and fails on a non-zero API code, which Feishu also returns
// with HTTP errors. out may be nil.
func (s *Sender) post(path, token string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, s.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		return err
	}
	var status struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw.Bytes(), &status); err != nil {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if status.Code != 0 {
		return fmt.Errorf("HTTP %d: code %d: %s", resp.StatusCode, status.Code, status.Msg)
	}
	if out != nil {
		return json.Unmarshal(raw.Bytes(), out)
	}
	return nil
}

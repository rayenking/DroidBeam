package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Webhook POSTs events to a configured URL. Delivery is asynchronous with a few retries.
// When a secret is set, the body is signed: X-DroidBeam-Signature: sha256=<hex hmac>.
type Webhook struct {
	url    string
	secret string
	client *http.Client
}

func newWebhook(url, secret string) *Webhook {
	return &Webhook{url: url, secret: secret, client: &http.Client{Timeout: 10 * time.Second}}
}

func (wh *Webhook) Send(event string, data any) {
	if wh == nil || wh.url == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"event":   event,
		"data":    data,
		"sent_at": time.Now().UTC(),
	})
	if err != nil {
		log.Printf("webhook: marshal: %v", err)
		return
	}
	go wh.deliver(event, payload)
}

func (wh *Webhook) deliver(event string, payload []byte) {
	const attempts = 4
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(1<<i) * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, wh.url, bytes.NewReader(payload))
		if err != nil {
			log.Printf("webhook: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-DroidBeam-Event", event)
		if wh.secret != "" {
			mac := hmac.New(sha256.New, []byte(wh.secret))
			mac.Write(payload)
			req.Header.Set("X-DroidBeam-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := wh.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			err = &httpStatusError{resp.StatusCode}
		}
		log.Printf("webhook %s attempt %d/%d failed: %v", event, i+1, attempts, err)
	}
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "HTTP " + http.StatusText(e.code) }

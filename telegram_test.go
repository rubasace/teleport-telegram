package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramWireContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid HTTP request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/sendMessage":
			if body["parse_mode"] != "HTML" {
				t.Error("message formatting is missing")
			}
			buttons := body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
			if buttons[0].(map[string]any)["callback_data"] != "a:nonce" {
				t.Error("missing approve callback")
			}
			if buttons[0].(map[string]any)["style"] != "success" || buttons[1].(map[string]any)["style"] != "danger" {
				t.Error("approval styles are missing")
			}
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"}}}`)
		case "/editMessageText":
			if body["parse_mode"] != "HTML" {
				t.Error("edited message formatting is missing")
			}
			if len(body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)) != 0 {
				t.Error("buttons not removed")
			}
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		case "/getUpdates":
			if body["offset"] != float64(123) {
				t.Error("offset not sent")
			}
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":123,"callback_query":{"id":"q","from":{"id":42},"data":"a:nonce","message":{"message_id":7,"date":1,"chat":{"id":42,"type":"private"}}}}]}`)
		default:
			t.Error("unexpected method")
		}
	}))
	defer server.Close()
	tg := &Telegram{url: server.URL + "/", client: server.Client()}
	id, err := tg.Send(context.Background(), 42, "<b>formatted safely upstream</b>", "nonce")
	if err != nil || id != 7 {
		t.Fatalf("send: %v", err)
	}
	if err := tg.Edit(context.Background(), 42, 7, "done"); err != nil {
		t.Fatal(err)
	}
	updates, err := tg.updates(context.Background(), 123)
	if err != nil || len(updates) != 1 || updates[0].Callback.From.ID != 42 {
		t.Fatalf("updates: %v", err)
	}
}

func TestTelegramErrorDoesNotLeakToken(t *testing.T) {
	tg := newTelegram("123:TOP_SECRET_TOKEN_THAT_MUST_NOT_LEAK")
	tg.url = "http://127.0.0.1:1/bot123:TOP_SECRET_TOKEN_THAT_MUST_NOT_LEAK/"
	err := tg.Answer(context.Background(), "id", "hello")
	if err == nil || strings.Contains(err.Error(), "TOP_SECRET") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestExistingWebhookFailsClosed(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"result":{"url":"https://existing.example"}}`)
	}))
	defer s.Close()
	tg := &Telegram{url: s.URL + "/", client: s.Client()}
	if tg.check(context.Background()) == nil {
		t.Fatal("existing webhook accepted")
	}
}

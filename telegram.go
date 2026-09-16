package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Message struct {
	ID   int64 `json:"message_id"`
	Date int64 `json:"date"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}
type Callback struct {
	ID   string `json:"id"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Message         *Message `json:"message"`
	InlineMessageID string   `json:"inline_message_id"`
	Data            string   `json:"data"`
}
type Update struct {
	ID       int64     `json:"update_id"`
	Callback *Callback `json:"callback_query"`
}

type Telegram struct {
	url    string
	client *http.Client
}

func newTelegram(token string) *Telegram {
	return &Telegram{url: "https://api.telegram.org/bot" + token + "/", client: &http.Client{
		Timeout:       40 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (t *Telegram) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.url+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := t.client.Do(req)
	// net/http errors contain the URL, which contains the bot token. Never
	// return them, nor Telegram's descriptions that can echo sensitive input.
	if err != nil {
		return errors.New("Telegram transport failed")
	}
	defer res.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Code        int             `json:"error_code"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&envelope); err != nil {
		return errors.New("invalid Telegram response")
	}
	// A retry after a lost response may edit the message to its existing text.
	if method == "editMessageText" && envelope.Code == 400 && envelope.Description == "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message" {
		return nil
	}
	// A deleted message has no buttons left to invalidate. Do not let it block
	// reconciliation and notification of every subsequent request.
	if method == "editMessageText" && envelope.Code == 400 && envelope.Description == "Bad Request: message to edit not found" {
		return nil
	}
	if res.StatusCode != 200 || !envelope.OK {
		return fmt.Errorf("Telegram %s failed (HTTP %d, code %d)", method, res.StatusCode, envelope.Code)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return errors.New("invalid Telegram result")
		}
	}
	return nil
}

func (t *Telegram) Send(ctx context.Context, chat int64, text, nonce string) (int64, error) {
	var result Message
	err := t.call(ctx, "sendMessage", map[string]any{
		"chat_id": chat, "text": text, "parse_mode": "HTML", "protect_content": true,
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]any{{
			{"text": "✅ Aprobar", "style": "success", "callback_data": "a:" + nonce},
			{"text": "⛔ Denegar", "style": "danger", "callback_data": "d:" + nonce},
		}}},
	}, &result)
	if err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != chat || result.Chat.Type != "private" {
		return 0, errors.New("Telegram returned an unexpected message destination")
	}
	return result.ID, nil
}

func (t *Telegram) Edit(ctx context.Context, chat, message int64, text string) error {
	return t.call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": message, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
		"reply_markup":         map[string]any{"inline_keyboard": [][]any{}}}, nil)
}
func (t *Telegram) Answer(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text, "show_alert": true}, nil)
}
func (t *Telegram) updates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := t.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"callback_query"}}, &updates)
	return updates, err
}

func (t *Telegram) check(ctx context.Context) error {
	var webhook struct {
		URL string `json:"url"`
	}
	if err := t.call(ctx, "getWebhookInfo", struct{}{}, &webhook); err != nil {
		return err
	}
	if webhook.URL != "" {
		return errors.New("bot has an existing webhook; dedicate a separate bot or remove it explicitly")
	}
	return nil
}

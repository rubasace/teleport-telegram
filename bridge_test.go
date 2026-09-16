package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	request     Request
	writes      int
	fail        bool
	unavailable bool
}

func (f *fakeBackend) List(context.Context) ([]Request, error) {
	if f.unavailable {
		return nil, errors.New("offline")
	}
	if f.request.ID == "" {
		return nil, nil
	}
	return []Request{f.request}, nil
}
func (f *fakeBackend) Get(context.Context, string) (Request, error) {
	if f.unavailable {
		return Request{}, errors.New("offline")
	}
	return f.request, nil
}
func (f *fakeBackend) Decide(_ context.Context, _, state, _ string) error {
	f.writes++
	if f.fail {
		return errors.New("lost response")
	}
	f.request.State = state
	return nil
}

type fakeMessenger struct {
	sent, edits int
	answer      string
	sendFailure bool
}

func (f *fakeMessenger) Send(context.Context, int64, string, string) (int64, error) {
	f.sent++
	if f.sendFailure {
		return 0, errors.New("offline")
	}
	return int64(f.sent), nil
}
func (f *fakeMessenger) Edit(context.Context, int64, int64, string) error { f.edits++; return nil }
func (f *fakeMessenger) Answer(_ context.Context, _, text string) error   { f.answer = text; return nil }

func fixture(t *testing.T) (*Bridge, *fakeBackend, *fakeMessenger, Callback) {
	t.Helper()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	f := &fakeBackend{request: Request{ID: "request-1", User: "agent", Reason: "Restart a pod", Roles: []string{"k8s-rw"}, State: "PENDING", Fingerprint: "original", RoleOnly: true, Created: now.Add(-time.Minute), Expires: now.Add(10 * time.Minute), AccessExpires: now.Add(14 * time.Minute)}}
	m := &fakeMessenger{}
	c := Config{StateFile: filepath.Join(t.TempDir(), "state.json"), Cluster: "example", UserID: 42, Requester: "agent", RoleScopes: map[string]string{"k8s-rw": "Restart and scale permitted workloads"}}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{config: c, state: s, backend: f, telegram: m, now: func() time.Time { return now }}
	if err := b.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := Callback{ID: "callback", Data: "a:" + s.Entries[f.request.ID].Nonce, Message: &Message{ID: 1, Date: now.Unix()}}
	q.From.ID = 42
	q.Message.Chat.ID = 42
	q.Message.Chat.Type = "private"
	return b, f, m, q
}

func TestRejectUnsafeCallbacks(t *testing.T) {
	tests := map[string]func(*Bridge, *fakeBackend, *Callback){
		"wrong sender":         func(b *Bridge, f *fakeBackend, q *Callback) { q.From.ID = 99 },
		"bot sender":           func(b *Bridge, f *fakeBackend, q *Callback) { q.From.IsBot = true },
		"wrong chat":           func(b *Bridge, f *fakeBackend, q *Callback) { q.Message.Chat.ID = 99 },
		"group chat":           func(b *Bridge, f *fakeBackend, q *Callback) { q.Message.Chat.Type = "group" },
		"forwarded message":    func(b *Bridge, f *fakeBackend, q *Callback) { q.Message.ID = 99 },
		"missing message":      func(b *Bridge, f *fakeBackend, q *Callback) { q.Message = nil },
		"inaccessible message": func(b *Bridge, f *fakeBackend, q *Callback) { q.Message.Date = 0 },
		"inline message":       func(b *Bridge, f *fakeBackend, q *Callback) { q.InlineMessageID = "inline" },
		"forged nonce":         func(b *Bridge, f *fakeBackend, q *Callback) { q.Data = "a:" + strings.Repeat("0", 32) },
		"unknown action":       func(b *Bridge, f *fakeBackend, q *Callback) { q.Data = "x:" + b.state.Entries[f.request.ID].Nonce },
		"modified request":     func(b *Bridge, f *fakeBackend, q *Callback) { f.request.Fingerprint = "changed" },
		"expired request":      func(b *Bridge, f *fakeBackend, q *Callback) { f.request.Expires = b.now() },
		"expired access":       func(b *Bridge, f *fakeBackend, q *Callback) { f.request.AccessExpires = b.now() },
		"long duration":        func(b *Bridge, f *fakeBackend, q *Callback) { f.request.AccessExpires = b.now().Add(time.Hour) },
		"unknown role":         func(b *Bridge, f *fakeBackend, q *Callback) { f.request.Roles = append(f.request.Roles, "admin") },
		"unknown requester":    func(b *Bridge, f *fakeBackend, q *Callback) { f.request.User = "other" },
		"resource request":     func(b *Bridge, f *fakeBackend, q *Callback) { f.request.RoleOnly = false },
		"approved elsewhere":   func(b *Bridge, f *fakeBackend, q *Callback) { f.request.State = "APPROVED" },
		"denied elsewhere":     func(b *Bridge, f *fakeBackend, q *Callback) { f.request.State = "DENIED" },
		"Teleport offline":     func(b *Bridge, f *fakeBackend, q *Callback) { f.unavailable = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			b, f, _, q := fixture(t)
			mutate(b, f, &q)
			if err := b.callback(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if f.writes != 0 {
				t.Fatal("unsafe callback reached approval API")
			}
		})
	}
}

func TestDecisionReplayAndRestart(t *testing.T) {
	for _, action := range []string{"a", "d"} {
		t.Run(action, func(t *testing.T) {
			b, f, m, q := fixture(t)
			q.Data = action + q.Data[1:]
			if err := b.callback(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			want := "APPROVED"
			if action == "d" {
				want = "DENIED"
			}
			if f.request.State != want || !strings.Contains(m.answer, want) {
				t.Fatal("wrong decision or feedback")
			}
			state, err := loadState(b.config)
			if err != nil {
				t.Fatal(err)
			}
			b.state = state
			if err := b.callback(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if err := b.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.writes != 1 || m.sent != 1 || m.edits != 1 {
				t.Fatalf("replay: writes=%d sends=%d edits=%d", f.writes, m.sent, m.edits)
			}
		})
	}
}

func TestAmbiguousMutationNeverRetried(t *testing.T) {
	b, f, _, q := fixture(t)
	f.fail = true
	if err := b.callback(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(b.config)
	if err != nil {
		t.Fatal(err)
	}
	b.state = state
	if err := b.callback(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatal("ambiguous decision replayed")
	}
	if err := b.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.state.Entries[f.request.ID].Final, "incierto") {
		t.Fatal("missing uncertain status")
	}
}

func TestStorageFailurePreventsMutation(t *testing.T) {
	b, f, _, q := fixture(t)
	b.config.StateFile = filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(b.config.StateFile, 0700); err != nil {
		t.Fatal(err)
	}
	err := b.callback(context.Background(), q)
	var storage fatalStorage
	if !errors.As(err, &storage) || f.writes != 0 {
		t.Fatalf("storage failure must stop before approval: %v", err)
	}
}

func TestRestartDeduplicatesPendingNotification(t *testing.T) {
	b, _, m, q := fixture(t)
	state, err := loadState(b.config)
	if err != nil {
		t.Fatal(err)
	}
	b.state = state
	if err := b.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.sent != 1 {
		t.Fatal("duplicate notification after restart")
	}
	if err := b.callback(context.Background(), q); err != nil {
		t.Fatal(err)
	}
}

func TestChangedRequestReplacesMessageAndInvalidatesOldButton(t *testing.T) {
	b, f, m, old := fixture(t)
	f.request.Fingerprint = "new"
	f.request.Reason = "Different operation"
	if err := b.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.sent != 2 || m.edits != 1 {
		t.Fatal("changed request was not replaced")
	}
	if err := b.callback(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatal("old prompt approved changed request")
	}
}

func TestReconcileOutageDoesNotInvalidatePrompts(t *testing.T) {
	b, f, _, _ := fixture(t)
	f.unavailable = true
	if b.reconcile(context.Background()) == nil {
		t.Fatal("missing outage error")
	}
	if b.state.Entries[f.request.ID].Final != "" {
		t.Fatal("outage mistaken for deletion")
	}
}

func TestPromptNotSilentlyTruncated(t *testing.T) {
	b, f, _, _ := fixture(t)
	f.request.Reason = strings.Repeat("x", 4000)
	if _, err := b.prompt(f.request); err == nil {
		t.Fatal("oversized prompt accepted")
	}
	if got := safeText("hello\n\u202Eworld"); got != "hello  world" {
		t.Fatalf("unsafe formatting: %q", got)
	}
}

func TestPromptEscapesUntrustedHTML(t *testing.T) {
	b, f, _, _ := fixture(t)
	f.request.Reason = `restart <b>everything</b> & then <a href="https://evil.example">approve</a>`
	f.request.User = `agent & <i>operator</i>`
	prompt, err := b.prompt(f.request)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"🛡️ <b>Solicitud de acceso</b>",
		"<blockquote>restart &lt;b&gt;everything&lt;/b&gt; &amp; then &lt;a href=&#34;https://evil.example&#34;&gt;approve&lt;/a&gt;</blockquote>",
		"<code>agent &amp; &lt;i&gt;operator&lt;/i&gt;</code>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "<blockquote>restart <b>everything</b>") {
		t.Fatal("request text was allowed to inject HTML")
	}
}

func TestRenderedFinalFormatsTrustedStatusAndEscapesUnknownState(t *testing.T) {
	if got := renderedFinal("Estado en Teleport: APPROVED"); got != "✅ <b>Aprobada en Teleport</b>" {
		t.Fatalf("approved status: %q", got)
	}
	if got := renderedFinal("Estado en Teleport: <b>forged</b>"); strings.Contains(got, "<b>forged</b>") {
		t.Fatalf("untrusted status was allowed to inject HTML: %q", got)
	}
}

func TestCorruptOrWrongDestinationStateFailsClosed(t *testing.T) {
	b, _, _, _ := fixture(t)
	c := b.config
	c.UserID++
	if _, err := loadState(c); err == nil {
		t.Fatal("wrong destination accepted")
	}
	if err := os.WriteFile(c.StateFile, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(b.config); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

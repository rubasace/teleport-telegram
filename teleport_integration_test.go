package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/types"
)

// Opt-in against a disposable local Community server; never production.
// The fixture must contain requester -> test-rw (15m), and bridge with only
// access_request [list, read, update]. Sign both users to the two files below.
func TestLocalTeleportCommunity(t *testing.T) {
	id := os.Getenv("TELEPORT_TEST_REQUEST_ID")
	if id == "" {
		t.Skip("requires disposable localhost Teleport fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := os.Getenv("TELEPORT_TEST_DIR")
	if dir == "" {
		t.Fatal("TELEPORT_TEST_DIR must point to the disposable fixture")
	}
	backend := &Teleport{address: "127.0.0.1:13025", identity: filepath.Join(dir, "bridge.pem")}
	requester := &Teleport{address: backend.address, identity: filepath.Join(dir, "requester.pem")}
	if err := requester.Decide(ctx, id, "APPROVED", "must be denied"); err == nil {
		t.Fatal("requester self-approved")
	}
	c, err := backend.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.GetUser(ctx, "requester", false); err == nil {
		t.Fatal("approver can read users")
	}
	if _, err := c.GetRoles(ctx); err == nil {
		t.Fatal("approver can read role definitions")
	}
	w, err := c.NewWatcher(ctx, types.Watch{Kinds: []types.WatchKind{{Kind: types.KindAccessRequest}}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	select {
	case <-w.Events():
	case <-ctx.Done():
		t.Fatal("watch did not initialize")
	}
	r, err := backend.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "PENDING" {
		t.Fatalf("expected a fresh pending request, got %s", r.State)
	}
	config := Config{Cluster: "approval-test", UserID: 42, Requester: "requester", RoleScopes: map[string]string{"test-rw": "Disposable fixture only"}, StateFile: filepath.Join(t.TempDir(), "state.json")}
	state, err := loadState(config)
	if err != nil {
		t.Fatal(err)
	}
	messenger := &fakeMessenger{}
	b := &Bridge{config: config, state: state, backend: backend, telegram: messenger, now: time.Now}
	if !b.eligible(r) {
		t.Fatalf("real request unexpectedly ineligible: %+v", r)
	}
	if err := b.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	e := state.Entries[id]
	if e == nil {
		t.Fatal("request not notified")
	}
	q := Callback{ID: "integration", Data: "a:" + e.Nonce, Message: &Message{ID: e.MessageID, Date: time.Now().Unix()}}
	q.From.ID = 42
	q.Message.Chat.ID = 42
	q.Message.Chat.Type = "private"
	if err := b.callback(ctx, q); err != nil {
		t.Fatal(err)
	}
	r, err = backend.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "APPROVED" {
		t.Fatalf("decision not applied: %s; feedback: %s", r.State, messenger.answer)
	}
	if err := b.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// The real server must preserve a denial against a later approval.
	if err := backend.Decide(ctx, id, "DENIED", "external administrator fixture"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Decide(ctx, id, "APPROVED", "must not override denial"); err == nil {
		t.Fatal("denial was overwritten")
	}
	// Teleport's list cache can lag a successful mutation; reconciliation must
	// converge rather than assume a write is immediately visible to list.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && state.Entries[id].Final != "Estado en Teleport: DENIED" {
		if err := b.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state.Entries[id].Final != "Estado en Teleport: DENIED" || !state.Entries[id].Rendered {
		t.Fatal("external denial not reflected in Telegram")
	}
}

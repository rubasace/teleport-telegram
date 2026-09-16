package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/types"
)

type Teleport struct{ address, identity string }

func (t *Teleport) dial(ctx context.Context) (*client.Client, error) {
	// Fresh Credentials on every connection: tbot renews the identity file.
	return client.New(ctx, client.Config{Addrs: []string{t.address}, Credentials: []client.Credentials{client.LoadIdentityFile(t.identity)}})
}

func convert(r types.AccessRequest) Request {
	raw, _ := json.Marshal(r)
	digest := sha256.Sum256(raw)
	return Request{ID: r.GetName(), User: r.GetUser(), Reason: r.GetRequestReason(), State: r.GetState().String(), Roles: r.GetRoles(), Created: r.GetCreationTime(), Expires: r.Expiry(), AccessExpires: r.GetAccessExpiry(), Fingerprint: hex.EncodeToString(digest[:]), RoleOnly: len(r.GetAllRequestedResourceIDs()) == 0 && !r.GetDryRun() && r.GetAssumeStartTime() == nil}
}

func (t *Teleport) List(ctx context.Context) ([]Request, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	requests, err := c.GetAccessRequests(ctx, types.AccessRequestFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(requests))
	for _, r := range requests {
		out = append(out, convert(r))
	}
	return out, nil
}

func (t *Teleport) Get(ctx context.Context, id string) (Request, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := t.dial(ctx)
	if err != nil {
		return Request{}, err
	}
	defer c.Close()
	requests, err := c.GetAccessRequests(ctx, types.AccessRequestFilter{ID: id})
	if err != nil {
		return Request{}, err
	}
	if len(requests) != 1 {
		return Request{}, errors.New("request missing")
	}
	return convert(requests[0]), nil
}

func (t *Teleport) Decide(ctx context.Context, id, state, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := t.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	s := types.RequestState_DENIED
	if state == "APPROVED" {
		s = types.RequestState_APPROVED
	} else if state != "DENIED" {
		return errors.New("unsupported decision")
	}
	return c.SetAccessRequestState(ctx, types.AccessRequestUpdate{RequestID: id, State: s, Reason: reason})
}

// Watches are hints; startup, reconnect and the periodic full list reconcile
// missed events. Rotate the connection every five minutes to reload tbot output.
func (t *Teleport) watch(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		func() {
			watchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			dialCtx, dialCancel := context.WithTimeout(watchCtx, 15*time.Second)
			c, err := t.dial(dialCtx)
			dialCancel()
			if err != nil {
				return
			}
			defer c.Close()
			w, err := c.NewWatcher(watchCtx, types.Watch{Kinds: []types.WatchKind{{Kind: types.KindAccessRequest}}})
			if err != nil {
				return
			}
			defer w.Close()
			for {
				select {
				case _, ok := <-w.Events():
					if !ok {
						return
					}
					select {
					case wake <- struct{}{}:
					default:
					}
				case <-w.Done():
					return
				case <-watchCtx.Done():
					return
				}
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

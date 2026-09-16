package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	AuthAddress  string            `json:"auth_address"`
	IdentityFile string            `json:"identity_file"`
	TokenFile    string            `json:"token_file"`
	StateFile    string            `json:"state_file"`
	Cluster      string            `json:"cluster"`
	UserID       int64             `json:"-"`
	Requester    string            `json:"requester"`
	RoleScopes   map[string]string `json:"role_scopes"`
}

func (c Config) validate() error {
	if c.AuthAddress == "" || c.IdentityFile == "" || c.TokenFile == "" || c.StateFile == "" || c.Cluster == "" || c.UserID <= 0 || c.Requester == "" || len(c.RoleScopes) == 0 {
		return errors.New("all configuration fields are required; TELEGRAM_APPROVER_ID must be positive")
	}
	for role, scope := range c.RoleScopes {
		if role == "" || scope == "" {
			return errors.New("every allowed role needs a scope description")
		}
	}
	return nil
}

type Request struct {
	ID, User, Reason, State, Fingerprint string
	Roles                                []string
	Created, Expires, AccessExpires      time.Time
	RoleOnly                             bool
}

type Backend interface {
	List(context.Context) ([]Request, error)
	Get(context.Context, string) (Request, error)
	Decide(context.Context, string, string, string) error
}

type Messenger interface {
	Send(context.Context, int64, string, string) (int64, error)
	Edit(context.Context, int64, int64, string) error
	Answer(context.Context, string, string) error
}

type Entry struct {
	Request   Request `json:"request"`
	Nonce     string  `json:"nonce"`
	MessageID int64   `json:"message_id"`
	// Persisted before the API mutation. An ambiguous failure cannot replay a decision.
	Attempted bool   `json:"attempted"`
	Final     string `json:"final,omitempty"`
	Rendered  bool   `json:"rendered"`
}

type State struct {
	Version int               `json:"version"`
	UserID  int64             `json:"user_id"`
	Cluster string            `json:"cluster"`
	Offset  int64             `json:"offset"`
	Entries map[string]*Entry `json:"entries"`
}

func loadState(c Config) (*State, error) {
	s := &State{Version: 1, UserID: c.UserID, Cluster: c.Cluster, Entries: map[string]*Entry{}}
	b, err := os.ReadFile(c.StateFile)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Version != 1 || s.UserID != c.UserID || s.Cluster != c.Cluster || s.Entries == nil {
		return nil, errors.New("state version or destination mismatch; migrate state explicitly")
	}
	return s, nil
}

// Same-directory rename and fsync make both the callback binding and the
// pre-mutation marker durable. Storage failures stop the process, never approve.
func saveState(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(s); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type Bridge struct {
	config   Config
	state    *State
	backend  Backend
	telegram Messenger
	now      func() time.Time
}

func (b *Bridge) eligible(r Request) bool {
	now := b.now()
	// Teleport samples creation and expiry at different instants. Allow one
	// second of calculation skew, but never extend either actual deadline.
	if !r.RoleOnly || r.User != b.config.Requester || r.State != "PENDING" || len(r.Roles) == 0 || r.Created.IsZero() || r.Created.After(now) || !r.Expires.After(now) || !r.AccessExpires.After(now) || r.AccessExpires.Sub(r.Created) > 15*time.Minute+time.Second {
		return false
	}
	for _, role := range r.Roles {
		if b.config.RoleScopes[role] == "" {
			return false
		}
	}
	return true
}

// Control removal plus HTML escaping prevents request-provided formatting and
// bidi controls from impersonating the trusted status/scope. No silent clipping:
// requests too large to show in full receive no actionable message.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

// htmlText is used for every value that did not originate in this binary.
// Requesters must not be able to turn their reason or an identifier into a
// trusted-looking heading, link, or button label.
func htmlText(s string) string { return html.EscapeString(safeText(s)) }

func (b *Bridge) prompt(r Request) (string, error) {
	var scopes []string
	for _, role := range r.Roles {
		scopes = append(scopes, fmt.Sprintf("• <b>%s</b>\n%s", htmlText(role), htmlText(b.config.RoleScopes[role])))
	}
	s := fmt.Sprintf("🛡️ <b>Solicitud de acceso</b>\n\n<b>Cluster</b> · <code>%s</code>\n<b>ID</b> · <code>%s</code>\n<b>Solicitante</b> · <code>%s</code>\n\n<b>Roles solicitados</b>\n%s\n\n<b>Motivo</b>\n<blockquote>%s</blockquote>\n\n<b>Ventana de decisión</b>\n<i>Creada</i> · <code>%s</code>\n<i>Responder antes de</i> · <code>%s</code>\n<i>Acceso hasta</i> · <code>%s</code>\n\n⚠️ <i>Se concede el rol completo, no sólo la operación descrita. Máximo 15 min.</i>", htmlText(b.config.Cluster), htmlText(r.ID), htmlText(r.User), strings.Join(scopes, "\n"), htmlText(r.Reason), r.Created.UTC().Format(time.RFC3339), r.Expires.UTC().Format(time.RFC3339), r.AccessExpires.UTC().Format(time.RFC3339))
	// Byte bound is conservative for Telegram's UTF-16 character limit and
	// reserves room for the final status appended later.
	if len(s) > 3500 {
		return "", errors.New("request too large to display safely")
	}
	return s, nil
}

func renderedFinal(final string) string {
	switch final {
	case "Estado en Teleport: APPROVED":
		return "✅ <b>Aprobada en Teleport</b>"
	case "Estado en Teleport: DENIED":
		return "⛔ <b>Denegada en Teleport</b>"
	case "Solicitud caducada o eliminada.":
		return "⌛ <b>Solicitud caducada o eliminada</b>"
	case "Solicitud caducada o fuera de la política del bridge.":
		return "⚠️ <b>Solicitud fuera de la política o caducada</b>"
	case "Solicitud modificada; revisar la nueva notificación.":
		return "🔄 <b>Solicitud modificada</b>\nRevisa la nueva notificación."
	case "Resultado incierto. Revisar en Teleport; no se reenviará la decisión.":
		return "⚠️ <b>Resultado incierto</b>\nRevísalo en Teleport; no se reenviará la decisión."
	default:
		return "ℹ️ " + htmlText(final)
	}
}

func (b *Bridge) persist() error { return saveState(b.config.StateFile, b.state) }

func (b *Bridge) reconcile(ctx context.Context) error {
	requests, err := b.backend.List(ctx)
	if err != nil {
		return err
	}
	current := make(map[string]Request, len(requests))
	for _, r := range requests {
		current[r.ID] = r
	}
	for id, e := range b.state.Entries {
		// A delayed response or an external denial may change a previously
		// displayed result. Always converge to Teleport's terminal state.
		if r, exists := current[id]; exists && r.State != "PENDING" {
			final := "Estado en Teleport: " + r.State
			if e.Final != final {
				e.Final = final
				e.Rendered = false
			}
		}
		if e.Final == "" {
			r, exists := current[id]
			switch {
			case !exists:
				e.Final = "Solicitud caducada o eliminada."
			case r.State != "PENDING":
				e.Final = "Estado en Teleport: " + r.State
			case !b.eligible(r):
				e.Final = "Solicitud caducada o fuera de la política del bridge."
			case r.Fingerprint != e.Request.Fingerprint:
				e.Final = "Solicitud modificada; revisar la nueva notificación."
			case e.Attempted:
				e.Final = "Resultado incierto. Revisar en Teleport; no se reenviará la decisión."
			}
		}
		if e.Final != "" && !e.Rendered && e.MessageID != 0 {
			prompt, _ := b.prompt(e.Request)
			if err := b.telegram.Edit(ctx, b.config.UserID, e.MessageID, prompt+"\n\n"+renderedFinal(e.Final)); err != nil {
				return err
			}
			e.Rendered = true
		}
		// Changed pending requests get a fresh nonce and a complete new prompt.
		if r, ok := current[id]; ok && e.Rendered && !e.Attempted && b.eligible(r) && r.Fingerprint != e.Request.Fingerprint {
			delete(b.state.Entries, id)
		}
		// Telegram keeps updates for 24h. Expired nonces never become valid again.
		if e.Rendered && b.now().After(e.Request.AccessExpires.Add(48*time.Hour)) {
			delete(b.state.Entries, id)
		}
	}
	if err := b.persist(); err != nil {
		return fatalStorage{err}
	}
	for _, r := range requests {
		if !b.eligible(r) {
			continue
		}
		prompt, err := b.prompt(r)
		if err != nil {
			slog.Warn("request cannot be displayed", "request", r.ID)
			continue
		}
		e, exists := b.state.Entries[r.ID]
		if !exists {
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return err
			}
			e = &Entry{Request: r, Nonce: hex.EncodeToString(nonce[:])}
			b.state.Entries[r.ID] = e
			if err := b.persist(); err != nil {
				return fatalStorage{err}
			}
		}
		if e.MessageID != 0 || e.Final != "" {
			continue
		}
		id, err := b.telegram.Send(ctx, b.config.UserID, prompt, e.Nonce)
		if err != nil {
			return err
		}
		e.MessageID = id
		if err := b.persist(); err != nil {
			return fatalStorage{err}
		}
		slog.Info("request notified", "request", r.ID)
	}
	return nil
}

type fatalStorage struct{ error }

func (b *Bridge) callback(ctx context.Context, q Callback) error {
	answer := func(s string) error { return b.telegram.Answer(ctx, q.ID, s) }
	if q.From.ID != b.config.UserID || q.From.IsBot || q.Message == nil || q.Message.Chat.ID != b.config.UserID || q.Message.Chat.Type != "private" || q.Message.Date == 0 || q.InlineMessageID != "" {
		return answer("No autorizado.")
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) != 2 || (parts[0] != "a" && parts[0] != "d") || len(parts[1]) != 32 {
		return answer("Botón no válido.")
	}
	var e *Entry
	for _, candidate := range b.state.Entries {
		if candidate.Nonce == parts[1] && candidate.MessageID == q.Message.ID {
			e = candidate
			break
		}
	}
	if e == nil || e.Final != "" || e.Attempted {
		return answer("Solicitud ya procesada o botón caducado.")
	}
	r, err := b.backend.Get(ctx, e.Request.ID)
	if err != nil {
		return answer("No se puede consultar Teleport. No se ha enviado ninguna decisión.")
	}
	if !b.eligible(r) || r.Fingerprint != e.Request.Fingerprint {
		return answer("La solicitud ha cambiado, caducado o ya está resuelta.")
	}
	state := "DENIED"
	if parts[0] == "a" {
		state = "APPROVED"
	}
	e.Attempted = true
	if err := b.persist(); err != nil {
		return fatalStorage{err}
	}
	reason := fmt.Sprintf("Telegram user_id=%d chat_id=%d message_id=%d decision=%s", q.From.ID, q.Message.Chat.ID, q.Message.ID, state)
	if err := b.backend.Decide(ctx, r.ID, state, reason); err != nil {
		slog.Error("decision outcome uncertain; automatic retry disabled", "request", r.ID)
		return answer("Resultado incierto. Revisar en Teleport antes de crear otra solicitud.")
	}
	slog.Info("decision submitted", "request", r.ID, "telegram_user_id", q.From.ID, "decision", state)
	// Report the authoritative post-write state; a concurrent denial wins.
	resolved, err := b.backend.Get(ctx, r.ID)
	if err != nil {
		return answer("Decisión enviada; pendiente de confirmar el estado en Teleport.")
	}
	if resolved.State == "PENDING" {
		return answer("Decisión enviada; pendiente de confirmar el estado en Teleport.")
	}
	e.Final = "Estado en Teleport: " + resolved.State
	if err := b.persist(); err != nil {
		return fatalStorage{err}
	}
	return answer(e.Final)
}

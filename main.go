package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	configPath := flag.String("config", "/config/config.json", "configuration file")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *configPath); err != nil {
		slog.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	var config Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&config)
	file.Close()
	if err != nil {
		return errors.New("invalid configuration JSON")
	}
	config.UserID, err = strconv.ParseInt(os.Getenv("TELEGRAM_APPROVER_ID"), 10, 64)
	if err != nil || config.UserID <= 0 {
		return errors.New("TELEGRAM_APPROVER_ID must be a positive numeric user ID")
	}
	if err := config.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config.StateFile), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(config.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another bridge holds the state lock")
	}
	state, err := loadState(config)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return errors.New("cannot read Telegram token file")
	}
	tokenString := strings.TrimSpace(string(token))
	if !validToken(tokenString) {
		return errors.New("invalid Telegram token format")
	}
	telegram := newTelegram(tokenString)
	if err := telegram.check(ctx); err != nil {
		return err
	}
	backend := &Teleport{address: config.AuthAddress, identity: config.IdentityFile}
	b := &Bridge{config: config, state: state, backend: backend, telegram: telegram, now: time.Now}
	if err := b.persist(); err != nil {
		return err
	}
	var lastSync, lastPoll atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Unix()-lastSync.Load() > 120 || time.Now().Unix()-lastPoll.Load() > 120 {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	defer server.Close()
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	go backend.watch(ctx, wake)
	// The polling goroutine waits for the durable offset before asking Telegram
	// to acknowledge an update. Only this event loop owns bridge state.
	batches := make(chan []Update)
	offsets := make(chan int64)
	go func(offset int64) {
		for ctx.Err() == nil {
			updates, err := telegram.updates(ctx, offset)
			if err != nil {
				slog.Warn("Telegram polling failed")
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			lastPoll.Store(time.Now().Unix())
			select {
			case batches <- updates:
			case <-ctx.Done():
				return
			}
			select {
			case offset = <-offsets:
			case <-ctx.Done():
				return
			}
		}
	}(state.Offset)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverError:
			return err
		case updates := <-batches:
			for _, update := range updates {
				if update.ID < state.Offset {
					continue
				}
				if update.Callback != nil {
					if err := b.callback(ctx, *update.Callback); err != nil {
						var storage fatalStorage
						if errors.As(err, &storage) {
							return err
						}
						slog.Warn("callback feedback failed")
					}
				}
				state.Offset = update.ID + 1
				if err := b.persist(); err != nil {
					return err
				}
			}
			select {
			case offsets <- state.Offset:
			case <-ctx.Done():
				return nil
			}
			if len(updates) > 0 {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		case <-ticker.C:
			select {
			case wake <- struct{}{}:
			default:
			}
		case <-wake:
			if err := b.reconcile(ctx); err != nil {
				var storage fatalStorage
				if errors.As(err, &storage) {
					return err
				}
				slog.Warn("reconciliation failed", "error", err)
			} else {
				lastSync.Store(time.Now().Unix())
			}
		}
	}
}

func validToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) < 20 {
		return false
	}
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return false
		}
	}
	for _, c := range parts[1] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

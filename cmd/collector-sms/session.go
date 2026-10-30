package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// stateDir: $TWALK_STATE_DIR, default $HERMES_HOME/secrets/twalk2/sms (0700).
func stateDir() string {
	if d := os.Getenv("TWALK_STATE_DIR"); d != "" {
		return d
	}
	home := os.Getenv("HERMES_HOME")
	if home == "" {
		home = "/home/hermes/hermes-new"
	}
	return filepath.Join(home, "secrets", "twalk2", "sms")
}

// loadSession reads session.json; (nil, nil) when there is none yet.
func loadSession(dir string) (*libgm.AuthData, error) {
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	ad := &libgm.AuthData{}
	if err := json.Unmarshal(b, ad); err != nil {
		return nil, fmt.Errorf("session.json illisible: %w", err)
	}
	return ad, nil
}

// saveSession writes AuthData (keys and tokens: secret) atomically, 0600, in a 0700 dir.
func saveSession(dir string, ad *libgm.AuthData) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	ad.CookiesLock.RLock()
	b, err := json.Marshal(ad)
	ad.CookiesLock.RUnlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-*.tmp") // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil && cerr == nil {
		return os.Rename(tmp.Name(), filepath.Join(dir, "session.json"))
	} else if err == nil {
		err = cerr
	}
	return err
}

// zlogSlog forwards libgm (zerolog) lines to slog keeping only level, message
// and error: libgm's other fields (and its debug/trace levels) can carry
// message payloads or identifiers, so they are dropped.
type zlogSlog struct{ l *slog.Logger }

func (w zlogSlog) Write(p []byte) (int, error) {
	var e struct{ Level, Message, Error string }
	if json.Unmarshal(p, &e) == nil {
		w.l.Warn("libgm: "+e.Message, "level", e.Level, "err", e.Error)
	}
	return len(p), nil
}

func libgmLogger(l *slog.Logger) zerolog.Logger {
	return zerolog.New(zlogSlog{l}).Level(zerolog.WarnLevel)
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// session is what `login` stores: a session token for instances that do not
// allow personal access tokens. Never logged.
type session struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	UserID string `json:"user_id"`
}

func sessionPath() string { return filepath.Join(stateDir(), "session.json") }

// loadSession fills MM_URL/MM_TOKEN from session.json when they are not set.
func loadSession() {
	if os.Getenv("MM_TOKEN") != "" {
		return
	}
	b, err := os.ReadFile(sessionPath())
	if err != nil {
		return
	}
	var s session
	if json.Unmarshal(b, &s) == nil && s.Token != "" {
		if os.Getenv("MM_URL") == "" {
			os.Setenv("MM_URL", s.URL)
		}
		os.Setenv("MM_TOKEN", s.Token)
	}
}

// errMFA is returned when the server requires a one-time code.
var errMFA = errors.New("mfa requis")

// apiLogin is POST /api/v4/users/login, the only write call: authentication.
func apiLogin(ctx context.Context, hc *http.Client, base, login, password, mfa string) (session, string, error) {
	body := map[string]string{"login_id": login, "password": password}
	if mfa != "" {
		body["token"] = mfa
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/v4/users/login", bytes.NewReader(b))
	if err != nil {
		return session{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return session{}, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &e)
		if strings.Contains(e.ID, "mfa") {
			return session{}, "", errMFA
		}
		return session{}, "", fmt.Errorf("connexion refusée (HTTP %d, %s)", resp.StatusCode, e.ID)
	}
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return session{}, "", err
	}
	tok := resp.Header.Get("Token")
	if tok == "" {
		return session{}, "", errors.New("le serveur n'a pas renvoyé de jeton de session")
	}
	return session{URL: strings.TrimRight(base, "/"), Token: tok, UserID: u.ID}, u.Username, nil
}

func saveSession(s session) error {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return os.WriteFile(sessionPath(), b, 0o600)
}

// login is interactive: URL, login, password (no echo), MFA code if asked.
func login(ctx context.Context) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("login doit être lancé dans un terminal interactif")
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
		if def != "" {
			prompt += " [" + def + "]"
		}
		fmt.Print(prompt + " : ")
		l, _ := in.ReadString('\n')
		if l = strings.TrimSpace(l); l == "" {
			return def
		}
		return l
	}
	secret := func(prompt string) string {
		fmt.Print(prompt + " : ")
		b, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimSpace(string(b))
	}
	base := ask("Adresse du serveur (https://…)", os.Getenv("MM_URL"))
	if !strings.HasPrefix(base, "https://") {
		return errors.New("adresse attendue en https://")
	}
	id := ask("Identifiant (email ou nom d'utilisateur)", "")
	pw := secret("Mot de passe")
	hc := &http.Client{Timeout: 30 * time.Second}
	s, user, err := apiLogin(ctx, hc, base, id, pw, "")
	if errors.Is(err, errMFA) {
		s, user, err = apiLogin(ctx, hc, base, id, pw, secret("Code de double authentification"))
	}
	if err != nil {
		return err
	}
	if err := saveSession(s); err != nil {
		return err
	}
	fmt.Printf("connecté en tant que @%s ; session dans %s\n", user, sessionPath())
	return nil
}

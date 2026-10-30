package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPILogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/users/login" {
			t.Errorf("appel inattendu %s %s", r.Method, r.URL.Path)
		}
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		switch {
		case b["password"] != "bon":
			w.WriteHeader(401)
			w.Write([]byte(`{"id":"api.user.login.invalid_credentials_email_username"}`))
		case b["token"] == "":
			w.WriteHeader(401)
			w.Write([]byte(`{"id":"mfa.validate_token.authenticate.app_error"}`))
		default:
			w.Header().Set("Token", "sess123")
			w.Write([]byte(`{"id":"u1","username":"mmaudet"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, _, err := apiLogin(ctx, srv.Client(), srv.URL, "mm", "faux", ""); err == nil || errors.Is(err, errMFA) {
		t.Fatalf("mauvais mot de passe: %v", err)
	}
	if _, _, err := apiLogin(ctx, srv.Client(), srv.URL, "mm", "bon", ""); !errors.Is(err, errMFA) {
		t.Fatalf("MFA attendu: %v", err)
	}
	s, user, err := apiLogin(ctx, srv.Client(), srv.URL+"/", "mm", "bon", "123456")
	if err != nil || s.Token != "sess123" || s.UserID != "u1" || user != "mmaudet" || s.URL != srv.URL {
		t.Fatalf("succès attendu: %+v %s %v", s, user, err)
	}
}

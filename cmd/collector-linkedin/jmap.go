package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Read-only JMAP client. Methods used: GET session, Mailbox/get (roles, to skip
// Trash and Spam), Email/query, Email/get. Never Email/set (so no $seen, no move).

var errLoggedOut = errors.New("grant Twake révoqué")

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

type jmapClient struct {
	hc         *http.Client
	sessionURL string
	tokenCmd   []string // prints a Bearer token on stdout; exit 78 = grant revoked
	tok        string
	api, acc   string
	account    string   // mailbox address, from the session
	skipBoxes  []string // Trash and Spam mailbox ids (nil = not resolved yet)
}

func newJMAP() *jmapClient {
	cmd := strings.Fields(os.Getenv("TWALK_TWAKE_TOKEN_CMD"))
	if len(cmd) == 0 {
		home := os.Getenv("HERMES_HOME")
		if home == "" {
			home = "/home/hermes/hermes-new"
		}
		cmd = []string{"node", filepath.Join(home, "tools", "twake-token", "twake-token.mjs"), "token"}
	}
	session := os.Getenv("TWALK_JMAP_SESSION_URL")
	if session == "" {
		session = "https://jmap-new.linagora.com/jmap/session"
	}
	return &jmapClient{hc: &http.Client{Timeout: time.Minute}, sessionURL: session, tokenCmd: cmd}
}

// refreshToken runs the broker; its stdout is the secret and is never logged.
func (c *jmapClient) refreshToken(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, c.tokenCmd[0], c.tokenCmd[1:]...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 78 {
			return errLoggedOut
		}
		return fmt.Errorf("jeton Twake indisponible : %w", err)
	}
	if c.tok = strings.TrimSpace(out.String()); c.tok == "" {
		return errors.New("jeton Twake vide")
	}
	return nil
}

// do sends one request; errors never carry the URL or the response body.
func (c *jmapClient) do(ctx context.Context, method, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return errors.New("requête invalide")
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return errors.New("réseau injoignable ou trop lent")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpStatusError(resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// call posts JMAP method calls and returns the arguments of each response.
func (c *jmapClient) call(ctx context.Context, calls ...[]any) ([]json.RawMessage, error) {
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	req := map[string]any{"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}, "methodCalls": calls}
	if err := c.do(ctx, "POST", c.api, req, &resp); err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, r := range resp.MethodResponses {
		if len(r) < 2 {
			return nil, errors.New("réponse JMAP invalide")
		}
		if bytes.Contains(r[0], []byte("error")) {
			var e struct{ Type string }
			_ = json.Unmarshal(r[1], &e)
			return nil, errors.New("erreur JMAP " + e.Type)
		}
		out = append(out, r[1])
	}
	return out, nil
}

// fetch returns the LinkedIn emails received at or after since, oldest first,
// skipping the ids for which skip is true. A fresh token is requested on each
// call; after a 401 once more, then the grant counts as lost.
func (c *jmapClient) fetch(ctx context.Context, since time.Time, skip func(id string) bool) ([]mail, error) {
	var lastErr error
	for try := 0; try < 2; try++ {
		if err := c.refreshToken(ctx); err != nil {
			return nil, err
		}
		ms, err := c.fetchOnce(ctx, since, skip)
		if st, ok := err.(httpStatusError); ok && (st == 401 || st == 403) {
			lastErr = err
			continue
		}
		return ms, err
	}
	return nil, fmt.Errorf("%w (%v après nouveau jeton)", errLoggedOut, lastErr)
}

func (c *jmapClient) fetchOnce(ctx context.Context, since time.Time, skip func(string) bool) ([]mail, error) {
	var s struct {
		APIURL, Username string
		PrimaryAccounts  map[string]string
	}
	if err := c.do(ctx, "GET", c.sessionURL, nil, &s); err != nil {
		return nil, err
	}
	if c.api, c.acc, c.account = s.APIURL, s.PrimaryAccounts["urn:ietf:params:jmap:mail"], s.Username; c.api == "" || c.acc == "" {
		return nil, errors.New("session JMAP sans apiUrl ou compte mail")
	}
	if c.skipBoxes == nil {
		r, err := c.call(ctx, []any{"Mailbox/get", map[string]any{"accountId": c.acc, "properties": []string{"id", "role"}}, "m"})
		if err != nil {
			return nil, err
		}
		var mb struct{ List []struct{ ID, Role string } }
		_ = json.Unmarshal(r[0], &mb)
		c.skipBoxes = []string{}
		for _, b := range mb.List {
			if b.Role == "trash" || b.Role == "junk" || b.Role == "spam" {
				c.skipBoxes = append(c.skipBoxes, b.ID)
			}
		}
	}
	filter := map[string]any{"from": "linkedin.com", "after": since.UTC().Format(time.RFC3339)}
	if len(c.skipBoxes) > 0 {
		filter["inMailboxOtherThan"] = c.skipBoxes
	}
	r, err := c.call(ctx, []any{"Email/query", map[string]any{"accountId": c.acc, "limit": 500,
		"filter": filter,
		"sort":   []map[string]any{{"property": "receivedAt", "isAscending": true}}}, "q"})
	if err != nil {
		return nil, err
	}
	var q struct{ IDs []string }
	if json.Unmarshal(r[0], &q) != nil {
		return nil, errors.New("réponse Email/query illisible")
	}
	var todo []string
	for _, id := range q.IDs {
		if !skip(id) {
			todo = append(todo, id)
		}
	}
	var out []mail
	for i := 0; i < len(todo); i += 50 { // the server caps full-body reads at 100 per call
		r, err := c.call(ctx, []any{"Email/get", map[string]any{"accountId": c.acc, "ids": todo[i:min(i+50, len(todo))],
			"properties":          []string{"id", "receivedAt", "from", "subject", "textBody", "bodyValues"},
			"fetchTextBodyValues": true, "maxBodyValueBytes": 4000}, "g"})
		if err != nil {
			return nil, err
		}
		var g struct {
			List []struct {
				ID, Subject, ReceivedAt string
				From                    []struct{ Name, Email string }
				TextBody                []struct{ PartID string }
				BodyValues              map[string]struct{ Value string }
			}
		}
		if json.Unmarshal(r[0], &g) != nil {
			return nil, errors.New("réponse Email/get illisible")
		}
		for _, e := range g.List {
			t, err := time.Parse(time.RFC3339, e.ReceivedAt)
			if err != nil {
				continue
			}
			m := mail{ID: e.ID, Received: t, Subject: e.Subject}
			if len(e.From) > 0 {
				m.Addr, m.From = strings.ToLower(e.From[0].Email), e.From[0].Name
			}
			for _, p := range e.TextBody {
				m.Body += e.BodyValues[p.PartID].Value + "\n"
			}
			out = append(out, m)
		}
	}
	return out, nil // the server sorts by receivedAt, and chunks keep that order
}

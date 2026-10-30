package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/util/dbutil"
	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

const (
	defaultUser   = "@mmaudet:linagora.com"
	deviceDisplay = "Twalk (Athena)"
)

// session is session.json (0600).
type session struct {
	UserID      id.UserID   `json:"user_id"`
	DeviceID    id.DeviceID `json:"device_id"`
	AccessToken string      `json:"access_token"`
	Homeserver  string      `json:"homeserver"`
}

var errLoggedOut = errors.New("session matrix absente ou expirée")

func loadSession(dir string) (*session, error) {
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, errLoggedOut
	}
	var s session
	if json.Unmarshal(b, &s) != nil || s.AccessToken == "" || s.UserID == "" || s.DeviceID == "" || s.Homeserver == "" {
		return nil, errLoggedOut
	}
	return &s, nil
}

func saveSession(dir string, s *session) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := filepath.Join(dir, ".session.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "session.json"))
}

// newClient never installs a logger (mautrix logs nothing by default, so no
// content can leak) and syncs with set_presence=offline.
func newClient(s *session) (*mautrix.Client, error) {
	cli, err := mautrix.NewClient(s.Homeserver, s.UserID, s.AccessToken)
	if err != nil {
		return nil, err
	}
	cli.DeviceID = s.DeviceID
	cli.SyncPresence = mevent.PresenceOffline
	return cli, nil
}

// pickleKey is the key that encrypts the Olm/Megolm pickles in crypto.db;
// generated once, stored 0600 next to the database.
func pickleKey(dir string) ([]byte, error) {
	p := filepath.Join(dir, "pickle.key")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return b, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	key := []byte(hex.EncodeToString(raw))
	return key, os.WriteFile(p, key, 0o600)
}

// openCrypto opens crypto.db with the pure-Go SQLite driver and initialises
// the Olm machine (goolm, build tag "goolm"). cryptohelper's own string-path
// shortcut needs the CGO driver "sqlite3-fk-wal", so the *dbutil.Database is
// built here from modernc.org/sqlite. Init uploads our device keys if the
// device has none yet, and replaces the client's sync store by the database
// (the sync token survives restarts).
func openCrypto(ctx context.Context, cli *mautrix.Client, dir string) (*cryptohelper.CryptoHelper, error) {
	key, err := pickleKey(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "crypto.db")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil { // create it 0600
		f.Close()
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		raw.Close()
		return nil, err
	}
	ch, err := cryptohelper.NewCryptoHelper(cli, key, db)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if err := ch.Init(ctx); err != nil {
		ch.Close()
		return nil, err
	}
	// Never answer room-key requests from other devices: we only read.
	ch.Machine().AllowKeyShare = func(context.Context, *id.Device, mevent.RequestedKeyInfo) *crypto.KeyShareRejection {
		return &crypto.KeyShareRejectNoResponse
	}
	return ch, nil
}

// noKeys: the device already has keys on the server that this store does not
// know (token pasted from another client): E2EE is impossible on it.
func noKeys(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not marked as shared")
}

func ask(in *bufio.Reader, prompt, def string) string {
	fmt.Print(prompt)
	s, _ := in.ReadString('\n')
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return def
}

func secret(prompt string) string {
	fmt.Print(prompt)
	b, _ := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return strings.TrimSpace(string(b))
}

// login is interactive; nothing it reads is ever logged.
func login(ctx context.Context, dir string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("login doit être lancé dans un terminal interactif")
	}
	in := bufio.NewReader(os.Stdin)
	user := id.UserID(ask(in, "Identifiant Matrix complet ["+defaultUser+"] : ", defaultUser))
	_, server, err := user.Parse()
	if err != nil {
		return fmt.Errorf("identifiant invalide: %w", err)
	}
	hs := "https://" + server
	if wk, err := mautrix.DiscoverClientAPI(ctx, server); err == nil && wk != nil && wk.Homeserver.BaseURL != "" {
		hs = wk.Homeserver.BaseURL
	}
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		return err
	}
	fmt.Println("Homeserver:", cli.HomeserverURL)
	cli.SyncPresence = mevent.PresenceOffline

	flows, err := cli.GetLoginFlows(ctx)
	if err != nil {
		return fmt.Errorf("flux de connexion: %w", err)
	}
	if flows.HasFlow(mautrix.AuthTypeSSO) && flows.HasFlow(mautrix.AuthTypeToken) {
		// SSO (LINAGORA: sso.linagora.com): the browser does the SSO login, then
		// Synapse redirects to redirectUrl?loginToken=…; the owner pastes that URL
		// here and we exchange the short-lived token for a NEW device of our own,
		// so end-to-end encryption works.
		redirect := "http://localhost:1/twalk-sso"
		u := strings.TrimRight(cli.HomeserverURL.String(), "/") + "/_matrix/client/v3/login/sso/redirect?redirectUrl=" + url.QueryEscape(redirect)
		fmt.Println("\n1. Ouvre cette adresse dans ton navigateur et connecte-toi (SSO) :\n\n   " + u)
		fmt.Println("\n2. Le navigateur finit sur une page en erreur « localhost » : c'est normal.\n   Copie l'adresse complète de cette page (elle contient loginToken=…) et colle-la ici.")
		raw := secret("\nAdresse de retour (saisie masquée) : ")
		tok := raw
		if pu, err := url.Parse(raw); err == nil && pu.Query().Get("loginToken") != "" {
			tok = pu.Query().Get("loginToken")
		}
		if tok == "" {
			return errors.New("loginToken absent de l'adresse collée")
		}
		_, err = cli.Login(ctx, &mautrix.ReqLogin{
			Type: mautrix.AuthTypeToken, Token: tok,
			InitialDeviceDisplayName: deviceDisplay, StoreCredentials: true,
		})
		if err != nil {
			return fmt.Errorf("connexion SSO refusée: %w", err)
		}
	} else if flows.HasFlow(mautrix.AuthTypePassword) {
		pw := secret("Mot de passe (saisie masquée) : ")
		_, err = cli.Login(ctx, &mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword, Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: string(user)},
			Password: pw, InitialDeviceDisplayName: deviceDisplay, StoreCredentials: true,
		})
		if err != nil {
			return fmt.Errorf("connexion refusée: %w", err)
		}
	} else {
		fmt.Println("Ce serveur n'accepte pas m.login.password (SSO ?). Alternative : coller le jeton d'accès d'un\nappareil dédié à Twalk et son device_id (Element: Paramètres > Aide > Jeton d'accès). Attention :\nun appareil qui a déjà des clés de chiffrement ne peut pas être déchiffré ici (rooms chiffrées illisibles).")
		cli.AccessToken = secret("Jeton d'accès (saisie masquée) : ")
		cli.DeviceID = id.DeviceID(ask(in, "device_id : ", ""))
		who, err := cli.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("jeton refusé: %w", err)
		}
		cli.UserID = who.UserID
		if who.DeviceID != "" {
			cli.DeviceID = who.DeviceID
		}
	}
	if cli.DeviceID == "" {
		return errors.New("device_id inconnu")
	}
	s := &session{UserID: cli.UserID, DeviceID: cli.DeviceID, AccessToken: cli.AccessToken, Homeserver: cli.HomeserverURL.String()}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := saveSession(dir, s); err != nil {
		return err
	}
	fmt.Printf("Connecté: %s, appareil %s\n", s.UserID, s.DeviceID)

	ch, err := openCrypto(ctx, cli, dir)
	if noKeys(err) {
		fmt.Println("Attention: cet appareil a déjà des clés ailleurs; chiffrement indisponible (rooms non chiffrées seulement).")
		return nil
	} else if err != nil {
		return fmt.Errorf("initialisation du chiffrement: %w", err)
	}
	defer ch.Close()
	if rk := secret("Clé de récupération pour vérifier l'appareil (Entrée pour passer) : "); rk != "" {
		if err := ch.Machine().VerifyWithRecoveryKey(ctx, rk); err != nil {
			fmt.Println("Vérification impossible (l'appareil reste non vérifié):", err)
		} else {
			fmt.Println("Appareil vérifié par cross-signing.")
		}
	}
	return nil
}

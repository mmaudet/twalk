package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/util/exhttp"
)

// Cookies used by Google Messages for web when paired with a Google account
// (same list as mautrix-gmessages' login.go). OSID is the messages.google.com one.
var (
	gaiaRequired = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}
	gaiaOptional = []string{"__Secure-1PSIDTS"}
)

// parseCookies accepts a JSON object {"SID": "..."}, a Cookie header, or a cURL
// command copied from the browser devtools ("Copy as cURL" on a request to
// messages.google.com). Only the Google Messages cookies are kept.
func parseCookies(raw string) (map[string]string, error) {
	out := map[string]string{}
	names := append(append([]string{}, gaiaRequired...), gaiaOptional...)
	var obj map[string]string
	if s := strings.TrimSpace(raw); strings.HasPrefix(s, "{") {
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, fmt.Errorf("JSON de cookies illisible")
		}
		for _, n := range names {
			if v := strings.TrimSpace(obj[n]); v != "" {
				out[n] = v
			}
		}
	} else {
		for _, n := range names { // boundary before the name: SID must not match HSID or __Secure-1PSID
			re := regexp.MustCompile(`(?:^|[\s;'"])` + regexp.QuoteMeta(n) + `=([^;'"\s]+)`)
			if m := re.FindStringSubmatch(raw); m != nil {
				out[n] = m[1]
			}
		}
	}
	var missing []string
	for _, n := range gaiaRequired {
		if out[n] == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cookies manquants : %s (copier une requête vers messages.google.com, connecté au compte Google)", strings.Join(missing, ", "))
	}
	return out, nil
}

// pairGoogle pairs with a Google account: cookies, then an emoji to tap on the
// phone. The cookies stay in session.json: libgm needs them on every request.
func pairGoogle(ctx context.Context, log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("pair-google", flag.ContinueOnError)
	file := fs.String("cookies", "", "fichier contenant les cookies (JSON, en-tête Cookie ou commande cURL) ; supprimé après succès")
	keep := fs.Bool("keep", false, "ne pas supprimer le fichier de cookies après succès")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return map[bool]int{true: 0, false: 64}[errors.Is(err, flag.ErrHelp)]
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "--cookies FICHIER requis (jamais les cookies en argument : ils finiraient dans l'historique)")
		return 64
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lecture des cookies :", err)
		return 1
	}
	cookies, err := parseCookies(string(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ad := libgm.NewAuthData()
	ad.SetCookies(cookies)
	cli := libgm.NewClient(ad, nil, libgmLogger(log), exhttp.SensibleClientSettings)
	cli.SetEventHandler(func(evt any) { log.Debug("event before pairing", "type", fmt.Sprintf("%T", evt)) })
	ctx, cancel := context.WithTimeout(ctx, pairTimeout)
	defer cancel()
	defer cli.Disconnect()

	if err := cli.FetchConfig(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Google refuse les cookies (session expirée ou incomplète ?) :", err)
		return 1
	}
	emoji, ps, err := cli.StartGaiaPairing(ctx, ctx)
	switch {
	case errors.Is(err, libgm.ErrNoDevicesFound):
		fmt.Fprintln(os.Stderr, "aucun téléphone trouvé sur ce compte Google : Google Messages doit être l'appli SMS du téléphone, connecté au même compte")
		return 1
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		fmt.Fprintln(os.Stderr, "le téléphone ne répond pas : vérifier qu'il est en ligne et réessayer")
		return 1
	case err != nil:
		fmt.Fprintln(os.Stderr, "démarrage de l'appairage :", err)
		return 1
	}
	fmt.Printf("\n    %s\n\nTouche cet emoji dans Google Messages sur le téléphone (image : %s).\n", emoji, libgm.GetEmojiSVG(emoji))
	if _, err := cli.FinishGaiaPairing(ctx, ps); err != nil {
		switch {
		case errors.Is(err, libgm.ErrIncorrectEmoji):
			fmt.Fprintln(os.Stderr, "mauvais emoji choisi sur le téléphone : relancer pair-google")
		case errors.Is(err, libgm.ErrPairingCancelled):
			fmt.Fprintln(os.Stderr, "appairage annulé sur le téléphone")
		case errors.Is(err, libgm.ErrPairingTimeout), errors.Is(err, context.DeadlineExceeded):
			fmt.Fprintln(os.Stderr, "délai dépassé : relancer pair-google")
		default:
			fmt.Fprintln(os.Stderr, "échec de l'appairage :", err)
		}
		return 1
	}
	if err := saveSession(stateDir(), ad); err != nil {
		log.Error("save session", "err", err)
		return 1
	}
	if !*keep {
		_ = os.Remove(*file)
	}
	fmt.Println("appairé ; session enregistrée dans", stateDir()+"/session.json")
	return 0
}

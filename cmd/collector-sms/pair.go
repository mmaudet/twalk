package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
)

const (
	pairTimeout = 5 * time.Minute
	qrLifetime  = 25 * time.Second // Google QR codes expire after ~30 s
	qrHint      = "Note : Google a désactivé l'appairage par QR (mautrix-gmessages v26.05) ; si le téléphone refuse le code, seul l'appairage par compte Google (cookies + emoji) subsiste dans libgm."
)

// pair links this collector as a Google Messages for web device by QR code and
// stores the resulting AuthData in session.json. Sends only the pairing handshake.
func pair(ctx context.Context, log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	qrFile := fs.String("qr", "/tmp/sms-qr.png", "fichier PNG du QR (512 px, 0600, réécrit à chaque rafraîchissement, supprimé à la fin)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return map[bool]int{true: 0, false: 64}[errors.Is(err, flag.ErrHelp)]
	}

	ad := libgm.NewAuthData()
	cli := libgm.NewClient(ad, nil, libgmLogger(log), exhttp.SensibleClientSettings)
	paired := make(chan *gmproto.PairedData, 1)
	onPair := func(d *gmproto.PairedData) {
		select {
		case paired <- d:
		default:
		}
	}
	cli.PairCallback.Store(&onPair) // set: libgm does not reconnect by itself after pairing
	cli.SetEventHandler(func(evt any) { log.Debug("event before pairing", "type", fmt.Sprintf("%T", evt)) })
	ctx, cancel := context.WithTimeout(ctx, pairTimeout)
	defer cancel()
	defer os.Remove(*qrFile)

	fail := func(what string, err error) int {
		log.Error(what, "err", err)
		fmt.Fprintln(os.Stderr, qrHint)
		return 1
	}
	if err := cli.FetchConfig(ctx); err != nil {
		return fail("fetch config", err)
	}
	qr, err := cli.StartLogin(ctx) // registers a phone relay and starts the pairing long poll
	if err != nil {
		return fail("start login", err)
	}
	show := func(data string) {
		q, err := qrcode.New(data, qrcode.Medium)
		if err != nil {
			log.Warn("qr", "err", err)
			return
		}
		if png, err := q.PNG(512); err == nil { // the QR carries session keys: 0600
			_ = os.WriteFile(*qrFile, png, 0o600)
		}
		fmt.Println(q.ToSmallString(false))
		fmt.Printf("QR écrit dans %s (valable ~%s). Téléphone : Google Messages > photo de profil > Appareils associés > Scanner le code QR\n", *qrFile, qrLifetime)
	}
	show(qr)

	tick := time.NewTicker(qrLifetime)
	defer tick.Stop()
	for {
		select {
		case <-paired:
			time.Sleep(2 * time.Second) // let the phone store the pairing before anyone reconnects
			cli.Disconnect()
			if err := saveSession(stateDir(), ad); err != nil {
				log.Error("save session", "err", err)
				return 1
			}
			fmt.Println("appairé ; session enregistrée dans", stateDir()+"/session.json")
			return 0
		case <-tick.C:
			if qr, err = cli.RefreshPhoneRelay(); err != nil {
				return fail("refresh qr", err)
			}
			show(qr)
		case <-ctx.Done():
			cli.Disconnect()
			fmt.Fprintln(os.Stderr, "échec de l'appairage : délai de 5 minutes dépassé ou interruption.", qrHint)
			return 1
		}
	}
}

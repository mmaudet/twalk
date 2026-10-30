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
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
)

// pair links this collector as an extra device of the owner's account, by
// phone-number code or by QR. It sends nothing but the pairing handshake.
func pair(ctx context.Context, log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	phone := fs.String("phone", "", "numéro E.164 sans + (ex. 33660469852)")
	qrFile := fs.String("qr", "", "fichier PNG du QR (réécrit à chaque rotation)")
	force := fs.Bool("force", false, "supprimer (localement) l'appareil déjà appairé")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return map[bool]int{true: 0, false: 64}[errors.Is(err, flag.ErrHelp)]
	}
	if (*phone == "") == (*qrFile == "") {
		fmt.Fprintln(os.Stderr, "pair : indiquer exactement un de --phone ou --qr")
		return 64
	}
	container, err := openStore(ctx, log)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	defer container.Close()
	old, err := pairedDevice(ctx, container)
	if err == nil && old != nil {
		if !*force {
			fmt.Fprintf(os.Stderr, "déjà appairé (%s) ; utiliser --force pour le remplacer\n", old.ID.User)
			return 1
		}
		err = old.Delete(ctx) // local deletion only; remove it in WhatsApp too
	}
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}

	// What the phone shows in its linked-devices list.
	store.SetOSInfo(displayName, [3]uint32{2, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
	cli := whatsmeow.NewClient(container.NewDevice(), slogAdapter{log})
	ctx, cancel := context.WithTimeout(ctx, pairTimeout)
	defer cancel()

	ev, failed := make(chan any, 16), make(chan error, 1)
	cli.AddEventHandler(func(raw any) {
		switch raw.(type) {
		case *events.QR, *events.PairSuccess, *events.PairError, *events.Connected:
			select {
			case ev <- raw:
			default:
			}
		}
	})
	// wait blocks until an event for which match returns true, or a failure/timeout.
	wait := func(match func(any) bool) (any, error) {
		for {
			select {
			case e := <-ev:
				if pe, ok := e.(*events.PairError); ok {
					return nil, pe.Error
				} else if match(e) {
					return e, nil
				}
			case err := <-failed:
				return nil, err
			case <-ctx.Done():
				return nil, errors.New("délai de 5 minutes dépassé")
			}
		}
	}
	if *qrFile != "" {
		qrc, err := cli.GetQRChannel(ctx)
		if err != nil {
			log.Error("qr channel", "err", err)
			return 1
		}
		go func() {
			for item := range qrc {
				if item.Event != whatsmeow.QRChannelEventCode {
					if item.Event != "success" {
						select {
						case failed <- fmt.Errorf("QR interrompu : %s %v", item.Event, item.Error):
						default:
						}
					}
					continue
				}
				if err := qrcode.WriteFile(item.Code, qrcode.Medium, 512, *qrFile); err != nil {
					log.Warn("write qr", "err", err)
				}
				if q, err := qrcode.New(item.Code, qrcode.Medium); err == nil {
					fmt.Println(q.ToSmallString(false))
				}
				fmt.Printf("QR écrit dans %s (valable ~%s)\n", *qrFile, item.Timeout.Round(time.Second))
			}
		}()
	}
	if err := cli.Connect(); err != nil {
		log.Error("connect", "err", err)
		return 1
	}
	defer cli.Disconnect()

	if *phone != "" {
		if _, err := wait(func(e any) bool { _, ok := e.(*events.QR); return ok }); err != nil { // websocket ready
			fmt.Fprintln(os.Stderr, "connexion impossible :", err)
			return 1
		}
		// The server validates "Browser (OS)"; fall back if our custom name is refused.
		code, err := cli.PairPhone(ctx, *phone, true, whatsmeow.PairClientChrome, displayName)
		if err != nil {
			log.Warn("custom display name refused, retrying as Chrome (Linux)", "err", err)
			code, err = cli.PairPhone(ctx, *phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		}
		if err != nil {
			log.Error("pair phone", "err", err)
			return 1
		}
		fmt.Println("Code d'appairage :", code)
		fmt.Println("WhatsApp > Appareils connectés > Connecter un appareil > Connecter avec un numéro de téléphone")
	}
	e, err := wait(func(e any) bool { _, ok := e.(*events.PairSuccess); return ok })
	if err != nil {
		fmt.Fprintln(os.Stderr, "échec de l'appairage :", err)
		return 1
	}
	// Let the post-pairing reconnect finish so the session is left clean.
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	for done := false; !done; {
		select {
		case c := <-ev:
			_, done = c.(*events.Connected)
		case <-wctx.Done():
			done = true
		}
	}
	cli.Disconnect()
	fmt.Printf("appairé (%s)\n", e.(*events.PairSuccess).ID.User)
	return 0
}

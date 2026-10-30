package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/skip2/go-qrcode"
)

const deviceName = "Twalk (Athena)"

var uriRE = regexp.MustCompile(`(?:sgnl|tsdevice):/?/?\S+`)

// link runs `signal-cli link`, shows its provisioning URI as a QR code (PNG
// file and text) and waits up to 5 minutes for the owner to scan it.
func (c *collector) link(ctx context.Context, qrPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, "--config", c.cfg, "link", "-n", deviceName)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer os.Remove(qrPath) // the URI is a one-shot secret
	shown := false
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if uri := uriRE.FindString(line); uri != "" && !shown {
			shown = true
			q, err := qrcode.New(uri, qrcode.Medium)
			if err == nil {
				var png []byte
				if png, err = q.PNG(512); err == nil {
					err = os.WriteFile(qrPath, png, 0o600)
				}
			}
			if err != nil {
				_ = cmd.Cancel()
				return fmt.Errorf("QR code: %w", err)
			}
			fmt.Println(q.ToSmallString(false))
			fmt.Printf("QR enregistré dans %s (0600).\nSur le téléphone: Signal > Réglages > Appareils liés > Associer un nouvel appareil, puis scanner (5 min).\n", qrPath)
		}
	}
	if err := cmd.Wait(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("délai de 5 minutes dépassé, rien n'est lié")
		}
		return err
	}
	if !shown {
		return errors.New("signal-cli n'a pas fourni d'URI de liaison")
	}
	c.loadAccount()
	fmt.Println("appareil lié au compte:", first(c.acct.Number, "(numéro inconnu)"))
	return nil
}

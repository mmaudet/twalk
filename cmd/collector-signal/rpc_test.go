package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSignalCli plays the child process: it reads requests on its stdin pipe,
// records every method, emits a receive notification before answering.
type fakeSignalCli struct {
	mu      sync.Mutex
	methods []string
}

func (f *fakeSignalCli) serve(stdin io.Reader, stdout io.WriteCloser) {
	defer stdout.Close()
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		var req struct {
			ID     int64
			Method string
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.mu.Unlock()
		io.WriteString(stdout, `{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+33612345678","timestamp":1,"dataMessage":{"timestamp":1,"message":"x"}}}}`+"\n")
		res := `[]`
		switch req.Method {
		case "listGroups":
			res = `[{"id":"R3JvdXA/+Zz==","name":"Équipe Twalk","isMember":true}]`
		case "listContacts":
			res = `[{"number":"+33612345678","uuid":"u1","name":"Alice Martin"},{"number":"+33600000000","uuid":"u2","name":""}]`
		}
		io.WriteString(stdout, `{"jsonrpc":"2.0","result":`+res+`,"id":`+strings.TrimSpace(string(rune('0'+req.ID)))+`}`+"\n")
	}
}

func TestRPCClientOnlyCallsWhitelistedMethods(t *testing.T) {
	inR, inW := io.Pipe()   // collector -> fake signal-cli
	outR, outW := io.Pipe() // fake signal-cli -> collector
	f := &fakeSignalCli{}
	go f.serve(inR, outW)

	rc := newRPC(inW, "")
	notified := make(chan rpcMsg, 16)
	loopDone := make(chan error, 1)
	go func() { loopDone <- rc.readLoop(outR, func(m rpcMsg) { notified <- m }) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &cache{rc: rc}
	if err := c.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if n, ok := c.contact("+33612345678", ""); !ok || n != "Alice Martin" {
		t.Errorf("contact %q %v", n, ok)
	}
	if _, ok := c.contact("+33600000000", "u2"); ok {
		t.Error("a contact without name is not known")
	}
	if c.groupName("R3JvdXA/+Zz==") != "Équipe Twalk" {
		t.Errorf("group %q", c.groupName("R3JvdXA/+Zz=="))
	}
	select {
	case m := <-notified:
		if m.Method != "receive" {
			t.Errorf("notification %q", m.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification routed")
	}

	for _, forbidden := range []string{"send", "sendReceipt", "sendTyping", "sendReaction", "updateProfile", "sendSyncRequest",
		"remoteDelete", "updateContact", "updateGroup", "quitGroup", "joinGroup", "sendMessageRequestResponse", "block", "version", ""} {
		var out any
		if err := rc.call(ctx, forbidden, &out); err == nil {
			t.Errorf("%q should be refused", forbidden)
		}
	}
	inW.Close()
	if err := <-loopDone; err != nil {
		t.Errorf("readLoop: %v", err)
	}
	if err := rc.call(ctx, "listGroups", new(any)); err == nil {
		t.Error("call after the stream ended must fail")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.methods) != 2 || f.methods[0] != "listGroups" || f.methods[1] != "listContacts" {
		t.Errorf("methods that reached signal-cli: %v", f.methods)
	}
	for m := range allowedMethods {
		if m != "listGroups" && m != "listContacts" {
			t.Errorf("unexpected allowed method %q", m)
		}
	}
}

func TestLinkURIRegexp(t *testing.T) {
	for _, line := range []string{"sgnl://linkdevice?uuid=abc-DEF_1&pub_key=Zm9v%2Bbar", "  QR: sgnl://linkdevice?uuid=x&pub_key=y  "} {
		if uriRE.FindString(line) == "" || strings.ContainsAny(uriRE.FindString(line), " \t") {
			t.Errorf("uri not found in %q", line)
		}
	}
	if uriRE.FindString("Associated with: +33612345678") != "" {
		t.Error("false positive")
	}
}

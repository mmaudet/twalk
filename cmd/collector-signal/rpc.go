package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// allowedMethods is the explicit white list of JSON-RPC methods this collector
// may ever send to signal-cli. Both are local reads of the account database.
// Anything else (send, sendReceipt, sendTyping, sendReaction, updateProfile,
// sendSyncRequest...) is refused before a single byte is written.
var allowedMethods = map[string]bool{"listGroups": true, "listContacts": true}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// rpcClient is a minimal JSON-RPC 2.0 client over a line-delimited stream.
// Calls are sequential (only two rare read methods exist).
type rpcClient struct {
	callMu  sync.Mutex
	w       io.Writer
	next    int64
	resp    chan rpcMsg
	done    chan struct{} // closed when the stream ends
	account string        // sent as "account" param when non-empty (multi-account daemon)
}

func newRPC(w io.Writer, account string) *rpcClient {
	return &rpcClient{w: w, resp: make(chan rpcMsg, 8), done: make(chan struct{}), account: account}
}

// call sends an allowed method and decodes its result into out.
func (c *rpcClient) call(ctx context.Context, method string, out any) error {
	if !allowedMethods[method] {
		return fmt.Errorf("méthode JSON-RPC interdite (lecture seule): %q", method)
	}
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.next++
	req := map[string]any{"jsonrpc": "2.0", "method": method, "id": c.next}
	if c.account != "" {
		req["params"] = map[string]string{"account": c.account}
	}
	b, _ := json.Marshal(req)
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // listGroups takes ~30 s with ~500 groups
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return io.ErrClosedPipe
		case m := <-c.resp:
			var id int64
			if json.Unmarshal(m.ID, &id) != nil || id != c.next {
				continue // late answer to an earlier, timed-out call
			}
			if m.Error != nil {
				return fmt.Errorf("%s: erreur %d", method, m.Error.Code)
			}
			return json.Unmarshal(m.Result, out)
		}
	}
}

// readLoop routes responses to the caller and hands notifications to notify,
// until r ends.
func (c *rpcClient) readLoop(r io.Reader, notify func(rpcMsg)) error {
	defer close(c.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method != "" {
			notify(m)
			continue
		}
		select {
		case c.resp <- m:
		default:
		}
	}
	return sc.Err()
}

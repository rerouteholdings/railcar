package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/zalando/go-keyring"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestAccountWrapVector(t *testing.T) {
	type wrapped struct {
		AD       string `json:"associated_data"`
		Commit   string `json:"commit"`
		CTHex    string `json:"ct_hex"`
		KeyHex   string `json:"key_hex"`
		NonceHex string `json:"nonce_hex"`
		Via      string `json:"via"`
	}
	var v struct {
		AccountKeyHex string `json:"account_key_hex"`
		AccountData   struct {
			AD        string `json:"associated_data"`
			CTHex     string `json:"ct_hex"`
			NonceHex  string `json:"nonce_hex"`
			Plaintext string `json:"plaintext"`
			Version   uint64 `json:"version"`
		} `json:"account_data"`
		Identifier string  `json:"identifier"`
		Staged     wrapped `json:"staged_wrapped_key"`
		Wrapped    wrapped `json:"wrapped_key"`
	}
	vectors.Load(t, "account.json", &v)
	// Account data, bound to its identity and version (§7.4).
	d := v.AccountData
	if ad := accountDataAD(v.Identifier, d.Version); string(ad) != d.AD {
		t.Errorf("account data: associated data %s", ad)
	}
	if ct, err := sealAESGCMNonce(vectors.Hex(t, v.AccountKeyHex), vectors.Hex(t, d.NonceHex), []byte(d.Plaintext), accountDataAD(v.Identifier, d.Version)); err != nil || hex.EncodeToString(ct) != d.CTHex {
		t.Errorf("account data: ct %x", ct)
	}
	for _, w := range []wrapped{v.Wrapped, v.Staged} {
		var commit []byte
		if w.Commit != "" {
			commit, _ = wire.DecodeB64(w.Commit)
		}
		ad := accountKeyAD(v.Identifier, w.Via, commit)
		if string(ad) != w.AD {
			t.Errorf("%s: associated data %s", w.Via, ad)
		}
		ct, err := sealAESGCMNonce(vectors.Hex(t, w.KeyHex), vectors.Hex(t, w.NonceHex), vectors.Hex(t, v.AccountKeyHex), ad)
		if err != nil || hex.EncodeToString(ct) != w.CTHex {
			t.Errorf("%s: ct %x", w.Via, ct)
		}
		got, err := unwrapAccountKey(WrappedKey{Via: w.Via, CT: ct, Commit: commit}, vectors.Hex(t, w.KeyHex), v.Identifier)
		if err != nil || hex.EncodeToString(got) != v.AccountKeyHex {
			t.Errorf("%s: unwrap %v", w.Via, err)
		}
		// A wrap served for another identity fails (§7.4).
		if _, err := unwrapAccountKey(WrappedKey{Via: w.Via, CT: ct, Commit: commit}, vectors.Hex(t, w.KeyHex), "tr:aaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
			t.Errorf("%s: opened for another identity", w.Via)
		}
	}
}

// fakeHome is a homeserver whose clock runs two hours ahead of the
// device's, to check that the client corrects for it (§8.4).
type fakeHome struct {
	srv    *httptest.Server
	ahead  time.Duration
	mu     sync.Mutex
	skews  int
	nonces map[string]bool
	chain  []wire.Signed
	stream func(*websocket.Conn)
}

func newFakeHome(t *testing.T) *fakeHome {
	f := &fakeHome{ahead: 2 * time.Hour, nonces: map[string]bool{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	now := func() int64 { return time.Now().Add(f.ahead).Unix() }
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", wire.MediaType)
		w.WriteHeader(status)
		if v != nil {
			b, _ := wire.Marshal(v)
			w.Write(b)
		}
	}
	skew := func(w http.ResponseWriter, ts int64) bool {
		if d := ts - now(); d > 600 || d < -600 {
			f.mu.Lock()
			f.skews++
			f.mu.Unlock()
			reply(w, 400, wire.Error{Code: wire.CodeClockSkew, Now: now(), Msg: fmt.Sprintf("%d s off", d)})
			return true
		}
		return false
	}
	mux.HandleFunc("GET /.well-known/tieroute/server", func(w http.ResponseWriter, r *http.Request) {
		// No Date header, so the client learns the clock from the API.
		w.Header()["Date"] = nil
		fmt.Fprintf(w, `{"name":"chat.example.com","roles":["home"],"handles":"chat.example.com","registration":"invite",
			"apis":[{"v":2,"client":"%[1]s/c/","s2s":"%[1]s/s/"}],"keys":[]}`, f.srv.URL)
	})
	mux.HandleFunc("POST /c/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Genesis wire.Signed `json:"genesis"`
			Invite  string      `json:"invite"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := wire.Unmarshal(body, &req); err != nil {
			reply(w, 400, wire.Error{Code: wire.CodeBadRequest})
			return
		}
		var e identity.Entry
		req.Genesis.Body(&e)
		if skew(w, e.TS) {
			return
		}
		if _, err := identity.Validate([]wire.Signed{req.Genesis}, identity.Options{}); err != nil {
			reply(w, 400, wire.Error{Code: wire.CodeInvalidChain})
			return
		}
		f.mu.Lock()
		f.chain = []wire.Signed{req.Genesis}
		f.mu.Unlock()
		reply(w, 200, map[string]string{"id": identity.IdentifierOf(req.Genesis)})
	})
	mux.HandleFunc("POST /c/auth/challenge", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := wire.EncodeB64([]byte(fmt.Sprintf("nonce-%07d", len(f.nonces))))
		f.nonces[n] = true
		f.mu.Unlock()
		reply(w, 200, map[string]any{"nonce": n, "expires": now() + 60, "now": now()})
	})
	mux.HandleFunc("POST /c/auth/session", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Auth wire.Signed `json:"auth"`
		}
		body, _ := io.ReadAll(r.Body)
		wire.Unmarshal(body, &req)
		var a AuthBody
		req.Auth.Body(&a)
		if skew(w, a.TS) {
			return
		}
		reply(w, 200, map[string]any{"token": "fake-token", "expires": now() + 30*86400})
	})
	mux.HandleFunc("POST /c/account/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			reply(w, 401, wire.Error{Code: wire.CodeUnauthenticated})
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /c/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
		if err != nil {
			return
		}
		f.stream(conn)
	})
	return f
}

func TestRegisterCorrectsTheClock(t *testing.T) {
	keyring.MockInit()
	f := newFakeHome(t)
	ctx := context.Background()
	opts := Options{Origins: map[string]string{"chat.example.com": f.srv.URL}}
	c, words, err := Register(ctx, "chat.example.com", "kat", "invite-code", filepath.Join(t.TempDir(), "p"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if words.Empty() || c.Handle() != "kat.chat.example.com" {
		t.Fatal("registration")
	}
	if f.skews != 1 {
		t.Errorf("%d clock-skew answers, want 1", f.skews)
	}
	skew, ok := c.Clock().Skew()
	if !ok || skew > -2*time.Hour+5*time.Second || skew < -2*time.Hour-5*time.Second {
		t.Errorf("skew %v", skew)
	}
	if c.state.PendingWrap != nil {
		t.Error("the recovery wrap wasn't stored")
	}
	if strings.Contains(fmt.Sprint(words), words.Words()[0]) {
		t.Error("the words print")
	}
}

func TestStreamReconnects(t *testing.T) {
	keyring.MockInit()
	f := newFakeHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := Options{Origins: map[string]string{"chat.example.com": f.srv.URL}}
	c, _, err := Register(ctx, "chat.example.com", "kat", "invite-code", filepath.Join(t.TempDir(), "p"), opts)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var auths []map[string]string
	pong := make(chan int64, 1)
	conns := 0
	f.stream = func(conn *websocket.Conn) {
		defer conn.CloseNow()
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()
		read := func() map[string]any {
			_, b, err := conn.Read(ctx)
			if err != nil {
				return nil
			}
			var m map[string]any
			wire.Unmarshal(b, &m)
			return m
		}
		first := read()
		a := map[string]string{}
		for k, v := range first {
			a[k], _ = v.(string)
		}
		mu.Lock()
		auths = append(auths, a)
		mu.Unlock()
		write := func(s string) { conn.Write(ctx, websocket.MessageText, []byte(s)) }
		write(`{"t":"ready","gap":false}`)
		switch n {
		case 1:
			write(`{"t":"item","item":{"cursor":"c1","ts":1,"notice":{"t":"chain-update","id":"tr:aaaaaaaaaaaaaaaaaaaaaaaa"}}}`)
			write(`{"t":"something-new"}`)
			conn.Close(websocket.StatusGoingAway, "restart")
		case 2:
			write(`{"t":"ping","n":7}`)
			if m := read(); m != nil && m["t"] == "pong" {
				pong <- int64(m["n"].(float64))
			}
			write(`{"t":"item","item":{"cursor":"c2","ts":2,"delivery":{"id":"AAAAAAAAAAAAAAAAAAAAAA","kind":"link","from":"tr:aaaaaaaaaaaaaaaaaaaaaaaa","to":["tr:aaaaaaaaaaaaaaaaaaaaaaaa"],"data":"AQID"}}}`)
			conn.Close(websocket.StatusCode(closeNotAllowed), "removed")
		}
	}
	var kinds []string
	var last Event
	for e := range c.Stream(ctx, StreamOptions{}) {
		kinds = append(kinds, e.Kind.String())
		last = e
	}
	want := "connected item disconnected connected item stopped"
	if strings.Join(kinds, " ") != want {
		t.Errorf("events %v, want %s", kinds, want)
	}
	if !errors.Is(last.Err, ErrDeviceRemoved) {
		t.Errorf("stopped with %v", last.Err)
	}
	if len(auths) != 2 || auths[0]["token"] != "fake-token" || auths[0]["after"] != "" || auths[1]["after"] != "c1" {
		t.Errorf("auth frames %v", auths)
	}
	select {
	case n := <-pong:
		if n != 7 {
			t.Errorf("pong %d", n)
		}
	default:
		t.Error("no pong")
	}
}

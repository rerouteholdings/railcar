package chat

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The end-to-end tests run against real rerouted servers, and the interop
// tests also against reroute-cli, Reroute's command-line client, as a
// black box. They're off by default. To run them:
//
//	(cd ../rerouted && go build -o /tmp/rerouted ./cmd/rerouted)
//	(cd ../reroute-desktop/core && cargo build -p reroute-cli)
//	RAILCAR_REROUTED=/tmp/rerouted \
//	RAILCAR_REROUTE_CLI=../reroute-desktop/core/target/debug/reroute-cli \
//	go test ./internal/chat -run E2E -v

// rerouteCLI is one reroute-cli profile: one identity on one device.
type rerouteCLI struct {
	t       *testing.T
	bin     string
	profile string
	env     []string
}

func newRerouteCLI(t *testing.T, name string) *rerouteCLI {
	t.Helper()
	bin := os.Getenv("RAILCAR_REROUTE_CLI")
	if bin == "" {
		t.Skip("set RAILCAR_REROUTE_CLI to a reroute-cli binary to run")
	}
	return &rerouteCLI{t: t, bin: bin, profile: filepath.Join(t.TempDir(), name)}
}

func (r *rerouteCLI) run(args ...string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, append([]string{"--profile", r.profile}, args...)...)
	cmd.Env = append(os.Environ(), r.env...)
	out, err := cmd.CombinedOutput()
	r.t.Logf("$ reroute-cli %s\n%s", strings.Join(args, " "), out)
	if err != nil {
		r.t.Fatalf("reroute-cli %s: %v", args[0], err)
	}
	return string(out)
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l[len(l)-1])
}

// railcar is one Railcar identity with its chat engine.
type railcar struct {
	t *testing.T
	e *Engine
}

func newRailcar(t *testing.T, origins map[string]string, server, handle string) *railcar {
	t.Helper()
	ctx := context.Background()
	c, _, err := client.Register(ctx, server, handle, testnet.SignupCode, filepath.Join(t.TempDir(), handle), client.Options{Origins: origins})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{}
	if os.Getenv("RAILCAR_E2E_LOGS") != "" {
		opts.Logger = slog.New(slog.NewTextHandler(testWriter{t, handle}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	e, err := New(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if err := e.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	return &railcar{t: t, e: e}
}

func (r *railcar) sync() {
	r.t.Helper()
	if err := r.e.Sync(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *railcar) chatWith(handle string) Chat {
	r.t.Helper()
	for _, c := range r.e.Contacts() {
		if c.Handle == handle && c.Chat != "" {
			ch, ok := r.e.Chat(c.Chat)
			if ok {
				return ch
			}
		}
	}
	r.t.Fatalf("no chat with @%s", handle)
	return Chat{}
}

func (r *railcar) find(chat, body string) Message {
	r.t.Helper()
	msgs, err := r.e.Messages(chat)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Body == body {
			return m
		}
	}
	r.t.Fatalf("no message %q in %v", body, msgs)
	return Message{}
}

func init() { keyring.MockInit() }

// testWriter sends a log to the test's output.
type testWriter struct {
	t    *testing.T
	name string
}

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s: %s", w.name, strings.TrimSpace(string(b)))
	return len(b), nil
}

// idOf finds a message's ID in reroute-cli's history, whose lines are
// "<id> <who>: <text>".
func idOf(t *testing.T, history, text string) string {
	t.Helper()
	for _, l := range strings.Split(history, "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), ": "+text) || strings.Contains(l, ": "+text+" ") {
			return strings.Fields(l)[0]
		}
	}
	t.Fatalf("no %q in reroute-cli's history", text)
	return ""
}

func settle() { time.Sleep(700 * time.Millisecond) }

// interop runs the interop scenario between a Railcar identity and a
// reroute-cli identity, on one server or two.
func interop(t *testing.T, katServer, raviServer string) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	names := []string{katServer}
	if raviServer != katServer {
		names = append(names, raviServer)
	}
	o := testnet.Start(t, bin, names...)
	ctx := context.Background()
	ravi.run("v2", "register", "ravi", "--server", o[raviServer], "--invite", testnet.SignupCode)
	raviHandle := "ravi." + raviServer
	kat := newRailcar(t, o, katServer, "kat")
	katHandle := "kat." + katServer

	// Kat adds Ravi with his invite code; Ravi accepts, and his app
	// creates the direct chat (§9.3).
	if _, err := kat.e.Add(ctx, lastLine(ravi.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", katHandle)
	settle()
	kat.sync()
	ch := kat.chatWith(raviHandle)
	if ch.Kind != KindDirect || ch.Hub != raviServer {
		t.Fatalf("the chat: %+v", ch)
	}

	// Messages both ways.
	if _, err := kat.e.SendText(ctx, ch.ID, "hello Ravi, from Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "hello Ravi, from Railcar") {
		t.Fatal("Ravi didn't get Kat's message")
	}
	ravi.run("send", katHandle, "hello Kat, from Reroute")
	settle()
	kat.sync()
	fromRavi := kat.find(ch.ID, "hello Kat, from Reroute")
	// A report of Reroute's message passes the homeserver's shape check,
	// and is forwarded to Ravi's home (§17.2, §17.3).
	if err := kat.e.Report(ctx, ch.ID, []string{fromRavi.ID}, "a test report", true); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.Report(ctx, ch.ID, []string{kat.find(ch.ID, "hello Ravi, from Railcar").ID}, "my own", false); err == nil {
		t.Error("Kat reported her own message")
	}

	// Ravi edits his message and reacts to Kat's; Kat sees both.
	hist := ravi.run("history", katHandle)
	katsID := idOf(t, hist, "hello Ravi, from Railcar")
	ravi.run("edit", katHandle, idOf(t, hist, "hello Kat, from Reroute"), "hello Kat, edited in Reroute")
	ravi.run("react", katHandle, katsID, "👍")
	settle()
	kat.sync()
	if m := kat.find(ch.ID, "hello Kat, edited in Reroute"); !m.Edited || m.ID != fromRavi.ID {
		t.Errorf("the edit: %+v", m)
	}
	if m := kat.find(ch.ID, "hello Ravi, from Railcar"); len(m.Reactions["👍"]) != 1 {
		t.Errorf("the reaction: %+v", m)
	}

	// Kat replies, edits her first message, reacts to Ravi's, and deletes
	// her reply; Ravi sees all of it.
	reply, err := kat.e.SendText(ctx, ch.ID, "a reply from Railcar", fromRavi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := kat.e.Edit(ctx, ch.ID, katsID, "hello Ravi, edited in Railcar"); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.React(ctx, ch.ID, fromRavi.ID, "🎉", false); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	hist = ravi.run("history", katHandle)
	for _, want := range []string{"hello Ravi, edited in Railcar", "a reply from Railcar", "🎉"} {
		if !strings.Contains(hist, want) {
			t.Errorf("Ravi's history doesn't show %q", want)
		}
	}
	if err := kat.e.Delete(ctx, ch.ID, reply.ID); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	if hist = ravi.run("history", katHandle); strings.Contains(hist, "a reply from Railcar") {
		t.Error("Ravi still shows the deleted reply")
	}

	// Ravi replaces his message key: his app commits a bare Update, which
	// Kat judges and applies, and his next message checks out against the
	// new key (§3.2.3).
	ravi.run("new-message-key")
	settle()
	kat.sync()
	ravi.run("send", katHandle, "signed with my new message key")
	settle()
	kat.sync()
	kat.find(ch.ID, "signed with my new message key")
	if c, _ := kat.e.Chat(ch.ID); c.Epoch < 2 || c.Stopped != "" {
		t.Fatalf("after the Update: %+v", c)
	}

	// Ravi links a second device; his app adds it to the chat (§11.9), and
	// it reads what Kat sends.
	desk := newRerouteCLI(t, "ravi-desk")
	linkCode := ""
	for _, l := range strings.Split(desk.run("v2", "link-start", raviHandle, "--server", o[raviServer]), "\n") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(l), "add-device "); ok {
			linkCode = c
		}
	}
	confirm := ""
	for _, l := range strings.Split(ravi.run("v2", "add-device", linkCode, "--name", "Ravi's desk"), "\n") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(l), "confirmation code: "); ok {
			confirm = c
		}
	}
	desk.run("v2", "link-confirm", confirm)
	ravi.run("v2", "approve-finish")
	desk.run("v2", "link-finish")
	for range 3 {
		ravi.run("sync")
		desk.run("sync")
		kat.sync()
		settle()
	}
	if _, err := kat.e.SendText(ctx, ch.ID, "hello to both of Ravi's devices", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := desk.run("sync"); !strings.Contains(out, "hello to both of Ravi's devices") {
		t.Error("Ravi's second device didn't get Kat's message")
	}
	desk.run("send", katHandle, "hello from Ravi's desk")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello from Ravi's desk")

	// The other way round: Ravi re-creates the chat, which stays the same
	// conversation (§11.11).
	ravi.run("restart", katHandle)
	settle()
	kat.sync()
	ch2 := kat.chatWith(raviHandle)
	if ch2.ID == ch.ID {
		t.Fatal("the chat wasn't re-created")
	}
	kat.find(ch2.ID, "hello Kat, edited in Reroute")
	if _, err := kat.e.SendText(ctx, ch2.ID, "after the restart", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "after the restart") {
		t.Error("Ravi didn't get Kat's message after the restart")
	}

	// A second Reroute identity adds Kat, and Railcar accepts: this time
	// Railcar creates the direct chat (§9.3).
	lee := newRerouteCLI(t, "lee")
	lee.run("v2", "register", "lee", "--server", o[raviServer], "--invite", testnet.SignupCode)
	leeHandle := "lee." + raviServer
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lee.run("add", code)
	settle()
	kat.sync()
	var leeID string
	for _, c := range kat.e.Contacts() {
		if c.Handle == leeHandle && c.State == StatePendingIn {
			leeID = c.ID
		}
	}
	if leeID == "" {
		t.Fatal("Kat doesn't see Lee's request")
	}
	lch, err := kat.e.Accept(ctx, leeID)
	if err != nil {
		t.Fatal(err)
	}
	if lch.Hub != katServer {
		t.Errorf("the hub is %s", lch.Hub)
	}
	settle()
	lee.run("sync")
	if _, err := kat.e.SendText(ctx, lch.ID, "hello Lee, in a chat Railcar made", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := lee.run("sync"); !strings.Contains(out, "hello Lee, in a chat Railcar made") {
		t.Fatal("Lee didn't get Kat's message")
	}
	lee.run("send", katHandle, "hello Kat, from Lee")
	settle()
	kat.sync()
	kat.find(lch.ID, "hello Kat, from Lee")

	// Lee commits in the chat Railcar created and hubs: Railcar's hub
	// takes the commit only if both derived the same epoch key (§11.6).
	lee.run("new-message-key")
	settle()
	kat.sync()
	if c, _ := kat.e.Chat(lch.ID); c.Epoch != 2 || c.Stopped != "" {
		t.Fatalf("after Lee's Update: %+v", c)
	}
	lee.run("send", katHandle, "from Lee, with a new message key")
	settle()
	kat.sync()
	kat.find(lch.ID, "from Lee, with a new message key")
}

func TestE2EInteropOneServer(t *testing.T) { interop(t, "a.test", "a.test") }

func TestE2EInteropTwoServers(t *testing.T) { interop(t, "a.test", "b.test") }

// waitEvent reads an engine's events until one matches.
func waitEvent(t *testing.T, e *Engine, what string, match func(Event) bool) Event {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-e.Events():
			if !ok {
				t.Fatalf("events closed waiting for %s", what)
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestE2ERailcar has two Railcar identities on two federated servers
// become contacts and chat. Ravi's side runs the live stream.
func TestE2ERailcar(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	done := make(chan error, 1)
	go func() { done <- ravi.e.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitEvent(t, ravi.e, "the stream", func(ev Event) bool { return ev.Kind == EventStream && ev.Stream == client.EventConnected })

	code, err := ravi.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, ravi.e, "Kat's request", func(ev Event) bool {
		return ev.Kind == EventContact && ev.Contact.State == StatePendingIn
	})
	if ev.Contact.Handle != "kat.a.test" {
		t.Fatalf("a request from %+v", ev.Contact)
	}
	rch, err := ravi.e.Accept(ctx, ev.Contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kch := kat.chatWith("ravi.b.test")
	if kch.ID != rch.ID || kch.Hub != "b.test" {
		t.Fatalf("Kat's chat %+v, Ravi's %+v", kch, rch)
	}
	if c, _ := kat.e.Chat(kch.ID); len(c.Members) != 2 {
		t.Fatalf("members %v", c.Members)
	}

	// Kat sends; Ravi's stream brings it.
	sent, err := kat.e.SendText(ctx, kch.ID, "hello Ravi, Railcar to Railcar", "")
	if err != nil || sent.Status != Sent {
		t.Fatal(sent, err)
	}
	waitEvent(t, ravi.e, "Kat's message", func(ev Event) bool {
		return ev.Kind == EventMessage && ev.New && ev.Message.Body == "hello Ravi, Railcar to Railcar"
	})
	// Typing reaches a connected device (§13.3).
	if err := kat.e.SetTyping(ctx, kch.ID, true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ravi.e, "Kat typing", func(ev Event) bool { return ev.Kind == EventTyping && ev.On })

	// Ravi answers, reacts, and edits; Kat catches up from the inbox.
	answer, err := ravi.e.SendText(ctx, rch.ID, "hello Kat", sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ravi.e.React(ctx, rch.ID, sent.ID, "👋", false); err != nil {
		t.Fatal(err)
	}
	if err := ravi.e.Edit(ctx, rch.ID, answer.ID, "hello Kat, edited"); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	if m := kat.find(kch.ID, "hello Kat, edited"); !m.Edited || m.Reply != sent.ID {
		t.Errorf("the answer: %+v", m)
	}
	if m := kat.find(kch.ID, "hello Ravi, Railcar to Railcar"); len(m.Reactions["👋"]) != 1 {
		t.Errorf("the reaction: %+v", m)
	}
	// An edit of someone else's message is refused locally.
	if err := kat.e.Edit(ctx, kch.ID, answer.ID, "not mine"); err == nil {
		t.Error("Kat edited Ravi's message")
	}
	if err := kat.e.Delete(ctx, kch.ID, sent.ID); err != nil {
		t.Fatal(err)
	}
	ev = waitEvent(t, ravi.e, "the delete", func(ev Event) bool {
		return ev.Kind == EventMessage && ev.Message.ID == sent.ID && ev.Message.Deleted
	})

	// History survives a restart: the profile opens again with its chats
	// and messages, and the MLS state still decrypts.
	dir := kat.e.Client().ProfileDir()
	kat.e.Close()
	c2, err := client.Open(dir, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := New(c2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	kat2 := &railcar{t: t, e: e2}
	kat2.find(kch.ID, "hello Kat, edited")
	if _, err := ravi.e.SendText(ctx, rch.ID, "after Kat's restart", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat2.sync()
	kat2.find(kch.ID, "after Kat's restart")
	if _, err := e2.SendText(ctx, kch.ID, "and back", ""); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ravi.e, "Kat's message after her restart", func(ev Event) bool {
		return ev.Kind == EventMessage && ev.New && ev.Message.Body == "and back"
	})
}

// TestE2EInteropDecline has each side decline the other's request (§9.3).
func TestE2EInteropDecline(t *testing.T) {
	bin := testnet.Rerouted(t)
	lee := newRerouteCLI(t, "lee")
	o := testnet.Start(t, bin, "a.test")
	ctx := context.Background()
	lee.run("v2", "register", "lee", "--server", o["a.test"], "--invite", testnet.SignupCode)
	kat := newRailcar(t, o, "a.test", "kat")

	// Lee declines Kat's request; Kat sees it.
	c, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite")))
	if err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("decline", "kat.a.test")
	settle()
	kat.sync()
	for _, k := range kat.e.Contacts() {
		if k.ID == c.ID && k.State != StateDeclined {
			t.Errorf("after Lee declined: %+v", k)
		}
	}

	// Kat declines Max's request; Max's app is told.
	max := newRerouteCLI(t, "max")
	max.run("v2", "register", "max", "--server", o["a.test"], "--invite", testnet.SignupCode)
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	max.run("add", code)
	settle()
	kat.sync()
	var maxID string
	for _, k := range kat.e.Contacts() {
		if k.Handle == "max.a.test" && k.State == StatePendingIn {
			maxID = k.ID
		}
	}
	if maxID == "" {
		t.Fatal("no request from Max")
	}
	if err := kat.e.Decline(ctx, maxID); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := max.run("sync"); !strings.Contains(strings.ToLower(out), "declined") {
		t.Error("Max's app didn't say Kat declined")
	}
}

// TestE2ERailcarMutual has two identities send each other requests before
// either sees the other's: they become contacts, and the one whose
// identifier sorts first creates the one direct chat (§9.2).
func TestE2ERailcarMutual(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "a.test", "ravi")
	katCode, _ := kat.e.Invite(ctx)
	raviCode, _ := ravi.e.Invite(ctx)
	if _, err := kat.e.Add(ctx, raviCode); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.Add(ctx, katCode); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		settle()
		kat.sync()
		ravi.sync()
	}
	k, r := kat.chatWith("ravi.a.test"), ravi.chatWith("kat.a.test")
	if k.ID != r.ID {
		t.Fatalf("two chats: %s and %s", k.ID, r.ID)
	}
	if _, err := kat.e.SendText(ctx, k.ID, "one chat", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	ravi.find(r.ID, "one chat")
}

// Package testnet runs local test networks of rerouted homeservers for
// Railcar's end-to-end tests. rerouted runs in its development mode, where
// servers reach each other at the origins they're given over plain HTTP.
// That mode is a rerouted feature, not protocol (§5.1).
package testnet

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SignupCode is the registration code every test server takes.
const SignupCode = "railcar-e2e-signup"

// CommunityCode is the creation code every test community server takes.
const CommunityCode = "railcar-e2e-community"

// Rerouted returns the rerouted binary named by RAILCAR_REROUTED, or skips
// the test.
func Rerouted(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RAILCAR_REROUTED")
	if bin == "" {
		t.Skip("set RAILCAR_REROUTED to a rerouted binary to run")
	}
	return bin
}

// FreePort returns a TCP port that's free now.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Start runs one rerouted for each server name, all federated with each
// other, and returns each name's origin. Each server gives out handles
// under its own name, such as kat.a.test on a.test.
func Start(t *testing.T, bin string, names ...string) map[string]string {
	return StartWith(t, bin, names, nil)
}

// StartWith is Start, with community server names as well: each of
// communities is served by the homeserver it maps to, on a listener of its
// own, and takes CommunityCode to create its community (§16.1). The
// result has every name's origin.
func StartWith(t *testing.T, bin string, names []string, communities map[string]string) map[string]string {
	t.Helper()
	origins := map[string]string{}
	ports := map[string]int{}
	var peers []string
	for _, n := range names {
		ports[n] = FreePort(t)
		origins[n] = fmt.Sprintf("http://127.0.0.1:%d", ports[n])
		peers = append(peers, n+"="+origins[n])
	}
	served := map[string][]string{}
	for c, home := range communities {
		origins[c] = fmt.Sprintf("http://127.0.0.1:%d", FreePort(t))
		peers = append(peers, c+"="+origins[c])
		served[home] = append(served[home], c+"="+origins[c])
	}
	for _, n := range names {
		start(t, bin, n, ports[n], origins[n], strings.Join(peers, ","), strings.Join(served[n], ","))
	}
	for c := range communities {
		wait(t, c, origins[c])
	}
	return origins
}

// CommunityURLs is what reroute-cli's REROUTE_COMMUNITY_URLS takes: each
// community server name and its origin.
func CommunityURLs(origins map[string]string, communities map[string]string) string {
	var out []string
	for c := range communities {
		out = append(out, c+"="+origins[c])
	}
	return strings.Join(out, ",")
}

func start(t *testing.T, bin, name string, port int, origin, peers, communities string) {
	t.Helper()
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	conf := fmt.Sprintf("REROUTED_NAME=%s\nREROUTED_HANDLES=%s\nREROUTED_LISTEN=127.0.0.1:%d\nREROUTED_PUBLIC_URL=%s\n"+
		"REROUTED_DEV=on\nREROUTED_DEV_PEERS=%s\nREROUTED_STORE=temp\nREROUTED_DATA_DIR=%s\n"+
		"REROUTED_REGISTRATION=invite\nREROUTED_REGISTRATION_CODE=%s\n",
		name, name, port, origin, peers, filepath.Join(dir, "data"), SignupCode)
	if communities != "" {
		conf += fmt.Sprintf("REROUTED_COMMUNITIES=%s\nREROUTED_COMMUNITY_CODE=%s\n", communities, CommunityCode)
	}
	if err := os.WriteFile(env, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-env", env)
	logf, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() || os.Getenv("RAILCAR_E2E_LOGS") != "" {
			b, _ := os.ReadFile(filepath.Join(dir, "log"))
			t.Logf("%s's log:\n%s", name, b)
		}
	})
	wait(t, name, origin)
}

func wait(t *testing.T, name, origin string) {
	t.Helper()
	for range 100 {
		if resp, err := http.Get(origin + "/.well-known/tieroute/server"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s didn't start", name)
}

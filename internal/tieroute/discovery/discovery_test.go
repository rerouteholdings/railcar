package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestServerDoc(t *testing.T) {
	k, err := sig.GenerateCompositeKey()
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ServerPath {
			http.NotFound(w, r)
			return
		}
		fetches++
		w.Header().Set("Cache-Control", "max-age=999999")
		fmt.Fprintf(w, `{"name":"chat.example.com","roles":["home","future"],"apis":[
			{"v":1,"client":"%[1]s/v1/client/","s2s":"%[1]s/v1/s2s/"},
			{"v":2,"client":"%[1]s/v2/client/","s2s":"%[1]s/v2/s2s/"}],
			"keys":[{"kid":"old","pub":"%[2]s","nbf":0,"exp":9999999999},
			        {"kid":"k1","pub":"%[3]s","nbf":0,"exp":9999999999}],
			"registration":"invite","unknown":true}`, base, wire.EncodeB64(k.EdPublic()), wire.EncodeB64(k.Public()))
	}))
	defer srv.Close()
	base = srv.URL
	r := &Resolver{Origins: map[string]string{"chat.example.com": srv.URL}}
	doc, err := r.ServerDoc(context.Background(), "chat.example.com")
	if err != nil {
		t.Fatal(err)
	}
	api, ok := doc.API(2)
	if !ok || api.Client != srv.URL+"/v2/client/" || !doc.HasRole(RoleHome) || doc.Date.IsZero() {
		t.Fatalf("%+v", doc)
	}
	if len(doc.Keys) != 1 || doc.Keys[0].Kid != "k1" {
		t.Errorf("keys %+v: the version 1 key should be dropped", doc.Keys)
	}
	if _, err := r.ServerDoc(context.Background(), "chat.example.com"); err != nil || fetches != 1 {
		t.Errorf("not cached: %d fetches", fetches)
	}
	if pub, err := r.Key(context.Background(), "chat.example.com", "k1", time.Now()); err != nil || len(pub) != sig.CompositePublicSize {
		t.Error(err)
	}
	if _, err := r.Key(context.Background(), "chat.example.com", "nope", time.Now()); err == nil {
		t.Error("found a key that isn't listed")
	}
	// Without the test-network override, plain HTTP bases are refused.
	if _, err := ParseServerDoc([]byte(`{"name":"a.example.com","roles":["home"],"apis":[{"v":2,"client":"http://a.example.com/c/","s2s":"https://a.example.com/s/"}],"keys":[]}`), "a.example.com", false); err == nil {
		t.Error("accepted a plain HTTP base")
	}
	for _, bad := range []string{
		`{"name":"b.example.com","roles":["home"],"apis":[{"v":2,"s2s":"https://a.example.com/s/"}],"keys":[]}`,
		`{"name":"a.example.com","roles":["home"],"apis":[{"v":2,"s2s":"https://a.example.com/s"}],"keys":[]}`,
		`{"name":"a.example.com","roles":["home"],"apis":[],"keys":[]}`,
	} {
		if _, err := ParseServerDoc([]byte(bad), "a.example.com", false); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	h := http.Header{}
	if cacheTTL(h) != defaultCache {
		t.Error("default")
	}
	h.Set("Cache-Control", "public, max-age=60")
	if cacheTTL(h) != time.Minute {
		t.Error("max-age")
	}
	h.Set("Cache-Control", "max-age=999999")
	if cacheTTL(h) != MaxCache {
		t.Error("cap")
	}
	h.Set("Cache-Control", "no-store")
	if cacheTTL(h) != 0 {
		t.Error("no-store")
	}
}

func TestParseTXT(t *testing.T) {
	id := "tr:7f3ka7qmx2vbn4rtd6wycs5e"
	rec, err := ParseTXT([][]string{{"v=spf1 -all"}, {"id=" + id + " ", "home=chat.example.com extra=1"}})
	if err != nil || rec.ID != id || rec.Home != "chat.example.com" {
		t.Fatalf("%+v %v", rec, err)
	}
	if _, err := ParseTXT([][]string{{"id=" + id + " home=a.example.com"}, {"id=tr:aaaaaaaaaaaaaaaaaaaaaaaa home=a.example.com"}}); err == nil {
		t.Error("accepted disagreeing records")
	}
	if _, err := ParseTXT([][]string{{"home=a.example.com"}}); err == nil {
		t.Error("accepted a record without id")
	}
	if _, err := ParseTXT([][]string{{"id=" + id}}); err == nil || !strings.Contains(err.Error(), "home") {
		t.Error("accepted a record without home")
	}
}

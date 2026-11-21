package discovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
)

// TestKeyRevocation checks the rules for server keys (§5.1): a key that
// claims more than 400 days is ignored, a revoked key doesn't count for
// what was signed at or after its revoked time, and a revocation stays
// when a later copy of the document leaves it out.
func TestKeyRevocation(t *testing.T) {
	k, err := sig.GenerateCompositeKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := base64.RawURLEncoding.EncodeToString(k.Public())
	now := time.Unix(1_800_000_000, 0)
	day := int64(24 * 60 * 60)
	doc := func(keys ...string) []byte {
		return []byte(`{"name":"hub.example.com","roles":["home"],"apis":[{"v":2,"client":"https://hub.example.com/c/","s2s":"https://hub.example.com/s/"}],"keys":[` + strings.Join(keys, ",") + `]}`)
	}
	key := func(kid string, nbf, exp, revoked int64) string {
		r := ""
		if revoked != 0 {
			r = fmt.Sprintf(`,"revoked":%d`, revoked)
		}
		return fmt.Sprintf(`{"kid":%q,"pub":%q,"nbf":%d,"exp":%d%s}`, kid, pub, nbf, exp, r)
	}
	served := doc(key("k1", now.Unix()-day, now.Unix()+300*day, now.Unix()-60), key("k2", now.Unix()-day, now.Unix()+500*day, 0))
	r := &Resolver{Now: func() time.Time { return now }, Via: func(context.Context, string) ([]byte, error) { return served, nil }}
	ctx := context.Background()
	if _, err := r.Refresh(ctx, "hub.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Key(ctx, "hub.example.com", "k2", now); err == nil {
		t.Error("a key over 400 days counted")
	}
	if !r.revoked("hub.example.com", "k1", now.Unix()) || r.revoked("hub.example.com", "k1", now.Unix()-120) {
		t.Error("revoked doesn't follow the signed time")
	}
	served = doc(key("k1", now.Unix()-day, now.Unix()+300*day, 0))
	if _, err := r.Refresh(ctx, "hub.example.com"); err != nil {
		t.Fatal(err)
	}
	if !r.revoked("hub.example.com", "k1", now.Unix()) {
		t.Error("a revocation came undone")
	}
}

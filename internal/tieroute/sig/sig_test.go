package sig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func key(t *testing.T, k vectors.Key) *CompositeKey {
	t.Helper()
	ed := k.Ed25519SeedHex
	if ed == "" {
		ed = k.RecoverySeedHex
	}
	ck, err := NewCompositeKey(vectors.Hex(t, k.MLDSASeedHex), vectors.Hex(t, ed))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ck.Public(), vectors.Hex(t, k.PublicHex)) {
		t.Fatal("composite public key differs from the vector")
	}
	return ck
}

func TestEd25519Vectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Name         string `json:"name"`
			MessageHex   string `json:"message_hex"`
			PublicHex    string `json:"public_hex"`
			SignatureHex string `json:"signature_hex"`
			Valid        bool   `json:"valid"`
		} `json:"cases"`
	}
	vectors.Load(t, "ed25519.json", &v)
	for _, c := range v.Cases {
		err := VerifyEd25519(vectors.Hex(t, c.PublicHex), vectors.Hex(t, c.MessageHex), vectors.Hex(t, c.SignatureHex))
		if (err == nil) != c.Valid {
			t.Errorf("%s: err = %v, want valid = %v", c.Name, err, c.Valid)
		}
	}
}

func TestCompositeVectors(t *testing.T) {
	var v struct {
		MessageRepresentative struct {
			MPrimeHex  string `json:"m_prime_hex"`
			MessageHex string `json:"message_hex"`
		} `json:"message_representative"`
		RFC struct {
			Ctx             string `json:"ctx"`
			Message         string `json:"message"`
			PKHex           string `json:"pk_hex"`
			SignatureHex    string `json:"signature_hex"`
			SignatureCtxHex string `json:"signature_with_ctx_hex"`
			SKHex           string `json:"sk_hex"`
		} `json:"rfc10073"`
	}
	vectors.Load(t, "v2/composite.json", &v)
	mr := v.MessageRepresentative
	if got := MessageRepresentative(vectors.Hex(t, mr.MessageHex)); !bytes.Equal(got, vectors.Hex(t, mr.MPrimeHex)) {
		t.Errorf("M' = %x", got)
	}
	// RFC 10073's known-answer test, with and without an application
	// context.
	pk := vectors.Hex(t, v.RFC.PKHex)
	sk := vectors.Hex(t, v.RFC.SKHex)
	k, err := NewCompositeKey(sk[:32], sk[32:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.Public(), pk) {
		t.Fatal("RFC 10073 key: public key differs")
	}
	if err := VerifyComposite(pk, []byte(v.RFC.Message), vectors.Hex(t, v.RFC.SignatureHex)); err != nil {
		t.Errorf("RFC 10073 signature: %v", err)
	}
	if err := verifyCompositeCtx(pk, []byte(v.RFC.Message), []byte(v.RFC.Ctx), vectors.Hex(t, v.RFC.SignatureCtxHex)); err != nil {
		t.Errorf("RFC 10073 signature with ctx: %v", err)
	}
	// Our own signatures verify, hedged or deterministic.
	for _, det := range []bool{false, true} {
		s, err := k.sign([]byte("hello"), det)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyComposite(pk, []byte("hello"), s); err != nil {
			t.Errorf("det=%v: %v", det, err)
		}
		if VerifyComposite(pk, []byte("hellO"), s) == nil {
			t.Errorf("det=%v: verified another message", det)
		}
	}
}

// verifyCompositeCtx verifies with a nonempty application context, which
// Tieroute never uses, for RFC 10073's known-answer test.
func verifyCompositeCtx(pub, m, ctx, signature []byte) error {
	mp := MessageRepresentative(m)
	h := mp[len(Prefix)+len(Label)+1:]
	mp = append(append(append(append([]byte(Prefix), Label...), byte(len(ctx))), ctx...), h...)
	pk, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pub[:MLDSAPublicSize])
	if err != nil {
		return err
	}
	if err := mldsa.Verify(pk, mp, signature[:MLDSASignatureSize], &mldsa.Options{Context: Label}); err != nil {
		return err
	}
	return VerifyEd25519(pub[MLDSAPublicSize:], mp, signature[MLDSASignatureSize:])
}

func TestSignedVectors(t *testing.T) {
	var v struct {
		Composite struct {
			EdSigHex    string `json:"ed25519_signature_hex"`
			MPrimeHex   string `json:"m_prime_hex"`
			MLDSASigHex string `json:"mldsa_signature_hex"`
		} `json:"composite"`
		MessageKey struct {
			PublicHex string `json:"public_hex"`
			SeedHex   string `json:"seed_hex"`
		} `json:"message_key"`
		MessageKeySigned vectors.Signed `json:"message_key_signed"`
		Reject           []struct {
			Note   string `json:"note"`
			Signed string `json:"signed"`
		} `json:"reject"`
		Signed vectors.Signed `json:"signed"`
		Signer vectors.Key    `json:"signer"`
	}
	vectors.Load(t, "v2/signed.json", &v)
	k := key(t, v.Signer)
	pub := k.Public()

	s, err := wire.ParseSigned([]byte(v.Signed.Signed))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDevice(s, pub); err != nil {
		t.Errorf("auth envelope: %v", err)
	}
	if got := MessageRepresentative(s.SigInput()); !bytes.Equal(got, vectors.Hex(t, v.Composite.MPrimeHex)) {
		t.Error("M' differs")
	}
	if !bytes.Equal(s.S[:MLDSASignatureSize], vectors.Hex(t, v.Composite.MLDSASigHex)) || !bytes.Equal(s.S[MLDSASignatureSize:], vectors.Hex(t, v.Composite.EdSigHex)) {
		t.Error("components differ")
	}
	// Deterministic signing reproduces the vector byte for byte.
	var body any
	if err := s.Body(&body); err != nil {
		t.Fatal(err)
	}
	again, err := SignDeviceDeterministic(k, s.T, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Canonical()) != v.Signed.Signed {
		t.Error("deterministic signing doesn't reproduce the vector")
	}

	// The message-key-signed app-message.
	ms, err := wire.ParseSigned([]byte(v.MessageKeySigned.Signed))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMessageKey(ms, vectors.Hex(t, v.MessageKey.PublicHex)); err != nil {
		t.Errorf("app-message: %v", err)
	}
	edKey := ed25519.NewKeyFromSeed(vectors.Hex(t, v.MessageKey.SeedHex))
	var mbody any
	_ = ms.Body(&mbody)
	if again, _ := SignMessageKey(edKey, ms.T, mbody); string(again.Canonical()) != v.MessageKeySigned.Signed {
		t.Error("message key signing doesn't reproduce the vector")
	}
	// A message key's signature can't stand in for a composite one.
	if VerifyDevice(ms, pub) == nil {
		t.Error("an Ed25519-only envelope verified as a device signature")
	}

	for _, r := range v.Reject {
		rs, err := wire.ParseSigned([]byte(r.Signed))
		if err != nil {
			continue // rejected while decoding
		}
		if VerifyDevice(rs, pub) == nil {
			t.Errorf("accepted: %s", r.Note)
		}
	}
}

func TestS2SVector(t *testing.T) {
	var v struct {
		Request vectors.Signed `json:"request"`
		DocKey  struct {
			Kid string `json:"kid"`
			Pub string `json:"pub"`
		} `json:"server_document_key"`
		ServerKey vectors.Key `json:"server_key"`
	}
	vectors.Load(t, "v2/s2s.json", &v)
	k := key(t, v.ServerKey)
	pub, err := wire.DecodeB64(v.DocKey.Pub)
	if err != nil || !bytes.Equal(pub, k.Public()) {
		t.Fatal("server document key differs")
	}
	s, err := wire.ParseSigned([]byte(v.Request.Signed))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyServer(s, v.DocKey.Kid, pub); err != nil {
		t.Error(err)
	}
	if VerifyServer(s, "k2", pub) == nil {
		t.Error("verified under another kid")
	}
}

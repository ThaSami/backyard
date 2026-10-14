package keys

import "testing"

func TestSignVerify(t *testing.T) {
	k1, k2 := Derive("s3cret-s3cret-s3cret-s3cret-s3cret", Deploy), Derive("s3cret-s3cret-s3cret-s3cret-s3cret", Upload)
	if string(k1) == string(k2) {
		t.Fatal("labels must yield different keys")
	}
	v := Sign(k1, []byte(`{"a":1}`))
	if p, err := Verify(k1, v); err != nil || string(p) != `{"a":1}` {
		t.Fatalf("verify: %v %q", err, p)
	}
	if _, err := Verify(k2, v); err == nil {
		t.Fatal("verify with wrong key must fail")
	}
	if _, err := Verify(k1, v+"x"); err == nil {
		t.Fatal("tampered value must fail")
	}
}

func TestLabelsAreDistinct(t *testing.T) {
	const secret = "s3cret-s3cret-s3cret-s3cret-s3cret"
	seen := map[string]string{}
	for _, label := range []string{Session, OAuthState, APIToken, Deploy, Upload, Anon, CLIConfirm} {
		k := string(Derive(secret, label))
		if prev, dup := seen[k]; dup {
			t.Fatalf("labels %q and %q derive the same key", prev, label)
		}
		seen[k] = label
	}
	if len(seen) != 7 {
		t.Fatalf("labels = %d", len(seen))
	}
}

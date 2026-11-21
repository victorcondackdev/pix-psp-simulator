package webhook

import (
	"errors"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("whsec_test")
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_800_000_000, 0)
	good := Sign(secret, now, body)
	other := Sign([]byte("other"), now, body)

	tests := []struct {
		name   string
		header string
		body   []byte
		tol    time.Duration
		now    time.Time
		want   error
	}{
		{"valid", good, body, 5 * time.Minute, now, nil},
		{"valid within tolerance", good, body, 5 * time.Minute, now.Add(4 * time.Minute), nil},
		{"clock skew backwards", good, body, 5 * time.Minute, now.Add(-4 * time.Minute), nil},
		{"tolerance disabled", good, body, 0, now.Add(24 * time.Hour), nil},
		{"rotation: second v1 matches", other + ",v1=" + good[len("t=1800000000,v1="):], body, time.Minute, now, nil},
		{"uppercase hex", "t=1800000000,v1=" + upper(good[len("t=1800000000,v1="):]), body, time.Minute, now, nil},
		{"spaces", "t=1800000000, v1=" + good[len("t=1800000000,v1="):], body, time.Minute, now, nil},
		{"expired", good, body, 5 * time.Minute, now.Add(6 * time.Minute), ErrTimestamp},
		{"tampered body", good, []byte(`{"id":"evt_2"}`), time.Minute, now, ErrSignature},
		{"wrong secret", other, body, time.Minute, now, ErrSignature},
		{"empty", "", body, time.Minute, now, ErrMissingHeader},
		{"no v1", "t=1800000000", body, time.Minute, now, ErrMalformedHeader},
		{"no t", "v1=abc", body, time.Minute, now, ErrMalformedHeader},
		{"bad t", "t=abc,v1=abc", body, time.Minute, now, ErrMalformedHeader},
		{"garbage", "nonsense", body, time.Minute, now, ErrMalformedHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyAt(secret, tt.header, tt.body, tt.tol, tt.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestVerifyUsesWallClock(t *testing.T) {
	secret := []byte("s")
	body := []byte("x")
	if err := Verify(secret, Sign(secret, time.Now(), body), body, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Verify(secret, Sign(secret, time.Now().Add(-time.Hour), body), body, time.Minute); !errors.Is(err, ErrTimestamp) {
		t.Fatalf("got %v", err)
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

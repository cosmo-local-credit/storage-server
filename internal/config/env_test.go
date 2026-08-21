package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"

	"github.com/cosmo-local-credit/storage-server/internal/api"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
)

func TestEnvOverrideMapsKeysAndValues(t *testing.T) {
	for name, tc := range map[string]struct {
		key     string
		value   string
		wantKey string
		wantVal any
	}{
		"scalar": {
			key: "STORAGE_API__MAX_BODY_SIZE", value: "16",
			wantKey: "api.max_body_size", wantVal: "16",
		},
		"nested section": {
			key: "STORAGE_S3__SECRET_ACCESS_KEY", value: "abc123",
			wantKey: "s3.secret_access_key", wantVal: "abc123",
		},
		// One bogus origin instead of eight was the failure this replaces.
		"comma list": {
			key: "STORAGE_API__ORIGIN", value: "https://a.test,https://b.test",
			wantKey: "api.origin", wantVal: []any{"https://a.test", "https://b.test"},
		},
		"space list": {
			key: "STORAGE_API__ALLOWED_FOLDERS", value: "voucher profile",
			wantKey: "api.allowed_folders", wantVal: []any{"voucher", "profile"},
		},
		"single element stays scalar": {
			key: "STORAGE_API__ORIGIN", value: "https://only.test",
			wantKey: "api.origin", wantVal: "https://only.test",
		},
	} {
		t.Run(name, func(t *testing.T) {
			gotKey, gotVal := EnvOverride(tc.key, tc.value)
			if gotKey != tc.wantKey {
				t.Fatalf("key = %q, want %q", gotKey, tc.wantKey)
			}
			if !reflect.DeepEqual(gotVal, tc.wantVal) {
				t.Fatalf("value = %#v, want %#v", gotVal, tc.wantVal)
			}
		})
	}
}

// A PEM block arrives from a docker env_file as one line with literal "\n"
// sequences. It must come back whole: unescaped, and never split on the spaces
// inside "-----BEGIN PUBLIC KEY-----".
func TestEnvOverrideRestoresAPemKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	block := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	key, value := EnvOverride("STORAGE_AUTH__PUBLIC_KEY", strings.ReplaceAll(block, "\n", "\\n"))
	if key != "auth.public_key" {
		t.Fatalf("key = %q", key)
	}
	restored, ok := value.(string)
	if !ok {
		t.Fatalf("value is %T, want a whole string", value)
	}
	if restored != block {
		t.Fatalf("value = %q, want the original PEM", restored)
	}
	// The consumer, not just the shape: a split or still-escaped key fails here.
	if _, err := api.LoadVerifyingKey(restored); err != nil {
		t.Fatalf("restored key does not load: %v", err)
	}
}

// End to end through koanf, because the provider is what decides whether these
// values reach the typed getters the service actually calls.
func TestEnvOverridesReachTypedGetters(t *testing.T) {
	t.Setenv("STORAGE_API__ORIGIN", "https://a.test,https://b.test")
	t.Setenv("STORAGE_API__ALLOWED_FOLDERS", "voucher,profile,pool,report,offering")
	t.Setenv("STORAGE_IMAGE__ALLOWED_WIDTHS", "400,800,1280")
	t.Setenv("STORAGE_API__MAX_BODY_SIZE", "16")
	t.Setenv("STORAGE_METRICS__ENABLE", "false")

	ko := koanf.New(".")
	if err := ko.Load(env.ProviderWithValue(EnvPrefix, ".", EnvOverride), nil); err != nil {
		t.Fatal(err)
	}

	if got := ko.MustStrings("api.origin"); len(got) != 2 {
		t.Fatalf("api.origin = %#v, want two entries", got)
	}
	if got := ko.MustStrings("api.allowed_folders"); len(got) != 5 {
		t.Fatalf("api.allowed_folders = %#v, want five entries", got)
	}
	if got := ko.MustInts("image.allowed_widths"); !reflect.DeepEqual(got, []int{400, 800, 1280}) {
		t.Fatalf("image.allowed_widths = %#v", got)
	}
	if got := ko.MustInt64("api.max_body_size"); got != 16 {
		t.Fatalf("api.max_body_size = %d, want 16", got)
	}
	if ko.Bool("metrics.enable") {
		t.Fatal("metrics.enable = true, want false")
	}
}

package mesh

import (
	"strings"
	"testing"
)

func TestNewTokenRoundTrip(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(tok.PSK) != pskBytes*2 { // hex
		t.Errorf("psk length = %d, want %d", len(tok.PSK), pskBytes*2)
	}
	if tok.Rendezvous == "" {
		t.Error("empty rendezvous")
	}
	encoded := tok.Encode()
	if encoded == "" {
		t.Fatal("empty encoded token")
	}
	got, err := DecodeToken(encoded)
	if err != nil {
		t.Fatalf("DecodeToken: %v", err)
	}
	if got.PSK != tok.PSK || got.Rendezvous != tok.Rendezvous {
		t.Errorf("round trip mismatch: %+v != %+v", got, tok)
	}
}

func TestNewTokenBootstrap(t *testing.T) {
	tok, err := NewToken("/ip4/1.2.3.4/tcp/4001/p2p/12D3KooWabc")
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(tok.BootstrapPeers) != 1 {
		t.Fatalf("bootstrap peers = %v, want 1", tok.BootstrapPeers)
	}
	// The embedded seed survives an encode/decode round trip.
	got, err := DecodeToken(tok.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BootstrapPeers) != 1 || got.BootstrapPeers[0] != "/ip4/1.2.3.4/tcp/4001/p2p/12D3KooWabc" {
		t.Errorf("bootstrap round trip = %v", got.BootstrapPeers)
	}
}

func TestNewTokenBadBootstrap(t *testing.T) {
	if _, err := NewToken("not-a-multiaddr"); err == nil {
		t.Error("expected error for invalid bootstrap multiaddr")
	}
	if _, err := NewToken("", "/ip4/9.9.9.9/tcp/1/p2p/12D3KooWxyz", "   "); err != nil {
		t.Errorf("whitespace/empty bootstrap should be skipped, got %v", err)
	}
}

func TestDecodeTokenErrors(t *testing.T) {
	cases := []string{
		"",
		"!!!not-base64!!!",
		// base64 of "{}" (missing rendezvous and psk)
		"e30",
		// base64 of {"rendezvous":"x"}
		"eyJyZW5kZXpvdXMiOiJ4In0",
	}
	for _, c := range cases {
		if _, err := DecodeToken(c); err == nil {
			t.Errorf("DecodeToken(%q): expected error", c)
		}
	}
}

func TestNewTokenDistinct(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a.PSK == b.PSK || a.Rendezvous == b.Rendezvous {
		t.Error("two tokens should differ")
	}
}

func TestConfigLoadSave(t *testing.T) {
	path := t.TempDir() + "/mesh.json"

	// Missing file yields an empty config, not an error.
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig missing: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("expected empty token, got %q", cfg.Token)
	}

	tok, _ := NewToken("/ip4/1.2.3.4/tcp/4001/p2p/12D3KooWabc")
	cfg.Name = "node-a"
	cfg.Roles = []string{"general", "researcher"}
	cfg.Cwds = map[string]string{"proj": "/tmp/proj"}
	cfg.Forwards = []ForwardRule{{Name: "docker", Mode: "expose", Local: "unix:///var/run/docker.sock", Enabled: true}}
	cfg.SetToken(tok)

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Name != "node-a" || len(got.Roles) != 2 || got.Cwds["proj"] != "/tmp/proj" {
		t.Errorf("config round trip mismatch: %+v", got)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].Name != "docker" {
		t.Errorf("forwards round trip: %+v", got.Forwards)
	}
	gotTok, err := got.TokenObj()
	if err != nil {
		t.Fatalf("TokenObj: %v", err)
	}
	if gotTok.PSK != tok.PSK || len(gotTok.BootstrapPeers) != 1 {
		t.Errorf("token round trip mismatch: %+v", gotTok)
	}

	if _, err := (&Config{}).TokenObj(); err != ErrNoToken {
		t.Errorf("empty config TokenObj = %v, want ErrNoToken", err)
	}
}

func TestValidateRule(t *testing.T) {
	valid := []ForwardRule{
		{Name: "a", Mode: "expose", Local: "127.0.0.1:8080", Enabled: true},
		{Name: "b", Mode: "expose", Local: "unix:///var/run/docker.sock"},
		{Name: "c", Mode: "listen", Bind: "127.0.0.1", Port: 9000, Peer: "12D3KooWx", Remote: "a"},
		{Name: "d", Mode: "listen", Bind: "unix:///tmp/s", Peer: "12D3KooWx", Remote: "a"}, // unix bind: no port needed
	}
	for _, r := range valid {
		if err := ValidateRule(r); err != nil {
			t.Errorf("ValidateRule(%+v): %v", r, err)
		}
	}
	invalid := []ForwardRule{
		{Mode: "expose", Local: "127.0.0.1:1"}, // no name
		{Name: "a", Mode: "bogus"},             // bad mode
		{Name: "a", Mode: "expose"},            // expose needs local
		{Name: "a", Mode: "listen", Bind: "127.0.0.1", Port: 0, Peer: "p", Remote: "r"},     // port 0
		{Name: "a", Mode: "listen", Bind: "127.0.0.1", Port: 70000, Peer: "p", Remote: "r"}, // port out of range
		{Name: "a", Mode: "listen", Bind: "127.0.0.1", Port: 8080, Remote: "r"},             // listen needs peer
		{Name: "a", Mode: "listen", Bind: "127.0.0.1", Port: 8080, Peer: "p"},               // listen needs remote
	}
	for _, r := range invalid {
		if err := ValidateRule(r); err == nil {
			t.Errorf("ValidateRule(%+v): expected error", r)
		}
	}
	if !strings.Contains(ValidateRule(invalid[0]).Error(), "name") {
		t.Errorf("missing name error: %v", ValidateRule(invalid[0]))
	}
}

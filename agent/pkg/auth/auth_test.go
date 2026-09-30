package auth

import (
	"testing"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/queue"
)

func TestPairingHandshake(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	origin := "https://pos.myrestaurant.com"
	code, expiresAt, err := authMgr.RequestPairing("My POS App", origin)
	if err != nil {
		t.Fatalf("RequestPairing failed: %v", err)
	}

	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %s", code)
	}

	if time.Now().After(expiresAt) {
		t.Errorf("code already expired")
	}

	// Confirm pairing
	pairedApp, err := authMgr.ConfirmPairing(code, origin)
	if err != nil {
		t.Fatalf("ConfirmPairing failed: %v", err)
	}

	if pairedApp.AppName != "My POS App" || pairedApp.Origin != origin {
		t.Errorf("unexpected paired app details: %+v", pairedApp)
	}

	if pairedApp.Token == "" {
		t.Fatalf("expected non-empty token")
	}

	// Validate token
	valid, app := authMgr.ValidateToken(pairedApp.Token, origin)
	if !valid || app == nil {
		t.Errorf("expected valid token validation, got valid=%t", valid)
	}

	// Validate bad token
	valid, _ = authMgr.ValidateToken("invalid_token", origin)
	if valid {
		t.Errorf("expected invalid token validation for bad token")
	}
}

func TestPairingInvalidCode(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	origin := "https://evil.com"
	_, _, err = authMgr.RequestPairing("Evil App", origin)
	if err != nil {
		t.Fatalf("RequestPairing failed: %v", err)
	}

	for i := 1; i <= 5; i++ {
		_, err = authMgr.ConfirmPairing("000000", origin)
		if err == nil {
			t.Errorf("expected error for wrong code on attempt %d", i)
		}
	}

	// 6th attempt should fail with MaxAttemptsExceeded
	_, err = authMgr.ConfirmPairing("000000", origin)
	if err != ErrMaxAttemptsExceeded && err != ErrPairingExpired {
		t.Errorf("expected ErrMaxAttemptsExceeded or ErrPairingExpired, got: %v", err)
	}
}

func TestTokenRevocation(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	origin := "https://store.example.com"
	code, _, _ := authMgr.RequestPairing("Store App", origin)
	app, err := authMgr.ConfirmPairing(code, origin)
	if err != nil {
		t.Fatalf("ConfirmPairing failed: %v", err)
	}

	valid, _ := authMgr.ValidateToken(app.Token, origin)
	if !valid {
		t.Fatalf("expected valid token before revocation")
	}

	if err := authMgr.RevokeApp(app.ID); err != nil {
		t.Fatalf("RevokeApp failed: %v", err)
	}

	valid, _ = authMgr.ValidateToken(app.Token, origin)
	if valid {
		t.Errorf("expected token to be invalid after revocation")
	}
}

func TestTokenIsBoundToItsOrigin(t *testing.T) {
	store, err := queue.NewStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer store.Close()

	authMgr, err := NewManager(store)
	if err != nil {
		t.Fatalf("failed to create auth manager: %v", err)
	}

	origin := "https://pos.myrestaurant.com"
	code, _, _ := authMgr.RequestPairing("My POS App", origin)
	app, err := authMgr.ConfirmPairing(code, origin)
	if err != nil {
		t.Fatalf("ConfirmPairing failed: %v", err)
	}

	if valid, _ := authMgr.ValidateToken(app.Token, origin); !valid {
		t.Fatal("expected the token to be valid for the origin it was issued to")
	}

	// A token handed to one web application must not be replayable by another
	// page that can also reach the loopback agent.
	if valid, _ := authMgr.ValidateToken(app.Token, "https://evil.example"); valid {
		t.Error("token must not be valid from a different origin")
	}

	// Non-browser clients normalise an absent Origin header to http://localhost,
	// which is a different origin to the one the token was issued to.
	if valid, _ := authMgr.ValidateToken(app.Token, "http://localhost"); valid {
		t.Error("token must not be valid from the loopback default origin")
	}
}

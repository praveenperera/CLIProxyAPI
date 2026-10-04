package executor

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

func TestTransportBindingKeepsHandshakeAndExecutionScope(t *testing.T) {
	ref, _ := affinity.NewCredentialRef("codex", "same-id", [32]byte{1})
	caller, _ := affinity.AuthenticatedCaller("host", "a")
	other, _ := affinity.AuthenticatedCaller("host", "b")
	chat, _ := affinity.NewChatID("same-chat")
	key, _ := affinity.NewChatKey("namespace", caller, chat)
	otherKey, _ := affinity.NewChatKey("namespace", other, chat)
	token := "Bearer original"
	ctx := WithTransportIdentity(context.Background(), ref, key)
	ctx = WithCredentialFence(ctx, func(r *http.Request) error {
		if r != nil && r.Header.Get("Authorization") != token {
			return affinity.NewError(affinity.CredentialChanged, time.Time{})
		}
		return nil
	})
	r, _ := http.NewRequest("GET", "wss://fake.invalid/responses", nil)
	r.Header.Set("Authorization", token)
	binding, err := NewTransportBinding(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer mutated")
	if err := binding.Check(ctx); err != nil {
		t.Fatal("handshake snapshot was mutable", err)
	}
	if err := binding.Check(WithTransportIdentity(ctx, ref, otherKey)); err == nil {
		t.Fatal("SDK caller collision reused transport")
	}
	replacement, _ := affinity.NewCredentialRef("codex", "same-id", [32]byte{2})
	if err := binding.Check(WithTransportIdentity(ctx, replacement, key)); err == nil {
		t.Fatal("replacement lineage reused transport")
	}
	// verified same-principal refresh keeps identity, but cannot claim the old wire token is new
	token = "Bearer refreshed"
	if err := binding.Check(ctx); err == nil {
		t.Fatal("manager identity hid old handshake credentials")
	}
}

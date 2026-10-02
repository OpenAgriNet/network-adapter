package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// peerKeyMgr is a key manager that CAN answer whether a caller is an admitted
// peer. The error case is included because the step must not fail a good call
// over it.
type peerKeyMgr struct {
	keyManagerStub
	isPeer bool
	err    error
}

func (k peerKeyMgr) IsAdmittedPeer(context.Context, string, string) (bool, error) {
	return k.isPeer, k.err
}

// plainKeyMgr cannot answer -- the case every key manager is in until it is
// extended, and the case a static-key deployment stays in forever.
type plainKeyMgr struct{ keyManagerStub }

// keyManagerStub satisfies definition.KeyManager. Only LookupNPKeys is ever
// reached by these tests; the rest exist to satisfy the interface.
type keyManagerStub struct{}

func (keyManagerStub) GenerateKeyset() (*model.Keyset, error) { return nil, nil }
func (keyManagerStub) InsertKeyset(context.Context, string, *model.Keyset) error {
	return nil
}
func (keyManagerStub) Keyset(context.Context, string) (*model.Keyset, error) { return nil, nil }
func (keyManagerStub) DeleteKeyset(context.Context, string) error            { return nil }
func (keyManagerStub) LookupNPKeys(context.Context, string, string) (string, string, error) {
	return "", "", nil
}

func TestVerifiedCallerIsRecordedFromTheSignature(t *testing.T) {
	step := &validateSignStep{km: plainKeyMgr{}}
	ctx := &model.StepContext{Context: context.Background()}

	step.recordVerifiedCaller(ctx, "mahavistara.oan.local", "op-1")

	if ctx.VerifiedCaller != "mahavistara.oan.local" {
		t.Fatalf("VerifiedCaller = %q, want the subscriber the signature proved", ctx.VerifiedCaller)
	}
}

// A key manager that cannot answer leaves the caller an ordinary participant.
// False is the safe direction: it keeps today's behaviour rather than silently
// narrowing what a caller can see.
func TestCallerIsNotAPeerWhenTheKeyManagerCannotTell(t *testing.T) {
	step := &validateSignStep{km: plainKeyMgr{}}
	ctx := &model.StepContext{Context: context.Background()}

	step.recordVerifiedCaller(ctx, "mahavistara.oan.local", "op-1")

	if ctx.VerifiedCallerPeerNetwork {
		t.Fatal("a caller was treated as a peer by a key manager that cannot tell")
	}
}

func TestAdmittedPeerIsRecordedAsOne(t *testing.T) {
	step := &validateSignStep{km: peerKeyMgr{isPeer: true}}
	ctx := &model.StepContext{Context: context.Background()}

	step.recordVerifiedCaller(ctx, "mahavistara.oan.local", "op-1")

	if !ctx.VerifiedCallerPeerNetwork {
		t.Fatal("an admitted peer was not recorded as one")
	}
}

// The case this exists for. Our own consumers and providers sign too, and this
// deployment's own network-layer adapter even carries role "network" -- none of
// them are peers, and treating them as such would cut them off from everything
// we crawled.
func TestOurOwnParticipantIsNotAPeer(t *testing.T) {
	step := &validateSignStep{km: peerKeyMgr{isPeer: false}}
	ctx := &model.StepContext{Context: context.Background()}

	step.recordVerifiedCaller(ctx, "network.bharatvistar.oan.local", "key-1")

	if ctx.VerifiedCallerPeerNetwork {
		t.Fatal("this network's own participant was treated as a foreign peer")
	}
}

// The signature has already verified when this runs, so the caller is
// legitimate. Failing to learn whether they are a peer must not reject the call.
func TestALookupFailureDoesNotRejectAVerifiedCaller(t *testing.T) {
	step := &validateSignStep{km: peerKeyMgr{err: errors.New("registry unreachable")}}
	ctx := &model.StepContext{Context: context.Background()}

	step.recordVerifiedCaller(ctx, "mahavistara.oan.local", "op-1")

	if ctx.VerifiedCaller != "mahavistara.oan.local" {
		t.Fatal("a registry failure lost the identity the signature had already proved")
	}
	if ctx.VerifiedCallerPeerNetwork {
		t.Fatal("a registry failure was read as 'is a peer'")
	}
}

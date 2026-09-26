package api

import (
	"errors"
	"testing"
	"time"

	"shpyrd/pkg/ext"
)

func TestExecTicketOneShot(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	id := ext.Identity{Subject: "u1", Email: "dev@example.test", Provider: "local"}
	code, err := st.mint(execTicket{Identity: id, Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	if code == "" {
		t.Fatal("mint returned an empty code")
	}
	got, err := st.redeem(code)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got.Project != "blog" || got.Instance != "web.1" || got.Identity.Email != "dev@example.test" {
		t.Errorf("redeemed = %+v", got)
	}
	// Replay is what the one-time code exists to stop: a second tab must not
	// be able to reuse a code it saw.
	if _, err := st.redeem(code); err == nil {
		t.Error("a redeemed ticket must not be redeemable again")
	}
}

func TestExecTicketExpires(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	now := time.Now()
	st.now = func() time.Time { return now }
	code, err := st.mint(execTicket{Project: "blog", Instance: "web.1"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(execTicketTTL + time.Second)
	if _, err := st.redeem(code); err == nil {
		t.Error("an expired ticket must be refused")
	}
}

func TestExecTicketUnknownCode(t *testing.T) {
	st := newTicketStore(execTicketTTL)
	if _, err := st.redeem("not-a-ticket"); err == nil {
		t.Error("an unknown code must be refused")
	}
	if _, err := st.redeem(""); err == nil {
		t.Error("an empty code must be refused")
	}
}

func TestShellRegistryOnePerUserPerProject(t *testing.T) {
	r := newShellRegistry()
	if !r.claim("u1", "blog") {
		t.Fatal("the first claim must succeed")
	}
	if r.claim("u1", "blog") {
		t.Error("a second shell for the same user and project must be refused")
	}
	// The limit is per user and per project, not global.
	if !r.claim("u2", "blog") || !r.claim("u1", "shop") {
		t.Error("other users and other projects are unaffected")
	}
	if !r.held("u1", "blog") {
		t.Error("held must report a live shell")
	}
	r.release("u1", "blog")
	if r.held("u1", "blog") {
		t.Error("held must be false after release")
	}
	if !r.claim("u1", "blog") {
		t.Error("the slot must be reusable once released")
	}
}

func TestActorKeyDistinguishesProviders(t *testing.T) {
	a := actorKey(ext.Identity{Subject: "u1", Provider: "local"})
	b := actorKey(ext.Identity{Subject: "u1", Provider: "github"})
	if a == b {
		t.Error("the same subject from two providers is two different people")
	}
}

func TestExecTicketStoreIsCapped(t *testing.T) {
	// Unredeemed tickets accumulate for a whole TTL window, and every mint
	// sweeps the map, so an uncapped store grows at quadratic cost under a
	// caller looping mint. The cap refuses clearly instead.
	st := newTicketStore(execTicketTTL)
	st.max = 3
	var codes []string
	for i := 0; i < st.max; i++ {
		code, err := st.mint(execTicket{Project: "blog", Instance: "web.1"})
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		codes = append(codes, code)
	}
	if _, err := st.mint(execTicket{Project: "blog", Instance: "web.1"}); !errors.Is(err, errTicketsFull) {
		t.Errorf("mint past the cap = %v, want errTicketsFull", err)
	}
	// Nothing is wedged: the cap counts outstanding tickets, so redeeming one
	// makes room, and the ones already minted still work.
	if _, err := st.redeem(codes[0]); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if _, err := st.mint(execTicket{Project: "blog", Instance: "web.1"}); err != nil {
		t.Errorf("mint after a redemption: %v", err)
	}
}

func TestExecTicketStoreRecoversWhenTicketsExpire(t *testing.T) {
	// The other way out of a full store is time: tickets live 30 seconds and the
	// sweep on each mint drops them, so a burst cannot lock the feature out.
	st := newTicketStore(execTicketTTL)
	st.max = 2
	now := time.Now()
	st.now = func() time.Time { return now }
	for i := 0; i < st.max; i++ {
		if _, err := st.mint(execTicket{Project: "blog", Instance: "web.1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.mint(execTicket{Project: "blog", Instance: "web.1"}); !errors.Is(err, errTicketsFull) {
		t.Fatalf("mint past the cap = %v, want errTicketsFull", err)
	}
	now = now.Add(execTicketTTL + time.Second)
	if _, err := st.mint(execTicket{Project: "blog", Instance: "web.1"}); err != nil {
		t.Errorf("mint once the outstanding tickets expired: %v", err)
	}
}

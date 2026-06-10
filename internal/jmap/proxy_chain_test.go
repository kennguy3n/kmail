package jmap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recInterceptor records that it was invoked and returns a fixed
// (intercepted, err) verdict.
type recInterceptor struct {
	id          string
	intercepted bool
	err         error
	seen        *[]string
}

func (r *recInterceptor) Intercept(_ context.Context, _ http.ResponseWriter, _ *http.Request, _ []byte) (bool, error) {
	*r.seen = append(*r.seen, r.id)
	return r.intercepted, r.err
}

func TestChainSendInterceptors_FilteringAndArity(t *testing.T) {
	// All-nil → nil chain.
	if got := ChainSendInterceptors(nil, nil); got != nil {
		t.Errorf("all-nil chain = %v, want nil", got)
	}
	// Single non-nil → returned directly (no wrapper allocation).
	seen := []string{}
	only := &recInterceptor{id: "a", seen: &seen}
	if got := ChainSendInterceptors(nil, only); got != only {
		t.Errorf("single chain should return the lone hook unwrapped")
	}
}

func TestChainSendInterceptors_FirstWins(t *testing.T) {
	seen := []string{}
	r := httptest.NewRequest("POST", "/jmap", nil)
	w := httptest.NewRecorder()

	// First hook declines, second intercepts → third never runs.
	a := &recInterceptor{id: "a", intercepted: false, seen: &seen}
	b := &recInterceptor{id: "b", intercepted: true, seen: &seen}
	c := &recInterceptor{id: "c", intercepted: true, seen: &seen}
	chain := ChainSendInterceptors(a, b, c)
	got, err := chain.Intercept(context.Background(), w, r, nil)
	if err != nil || !got {
		t.Fatalf("Intercept = (%v,%v) want (true,nil)", got, err)
	}
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Errorf("call order = %v, want [a b] (c must not run)", seen)
	}
}

func TestChainSendInterceptors_ErrorShortCircuits(t *testing.T) {
	seen := []string{}
	r := httptest.NewRequest("POST", "/jmap", nil)
	w := httptest.NewRecorder()
	boom := errors.New("hook failure")

	a := &recInterceptor{id: "a", err: boom, seen: &seen}
	b := &recInterceptor{id: "b", intercepted: true, seen: &seen}
	chain := ChainSendInterceptors(a, b)
	got, err := chain.Intercept(context.Background(), w, r, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got {
		t.Errorf("intercepted = true, want false on error")
	}
	if len(seen) != 1 || seen[0] != "a" {
		t.Errorf("call order = %v, want [a] (error short-circuits)", seen)
	}
}

func TestChainSendInterceptors_AllDecline(t *testing.T) {
	seen := []string{}
	r := httptest.NewRequest("POST", "/jmap", nil)
	w := httptest.NewRecorder()
	a := &recInterceptor{id: "a", seen: &seen}
	b := &recInterceptor{id: "b", seen: &seen}
	chain := ChainSendInterceptors(a, b)
	got, err := chain.Intercept(context.Background(), w, r, nil)
	if got || err != nil {
		t.Fatalf("Intercept = (%v,%v) want (false,nil)", got, err)
	}
	if len(seen) != 2 {
		t.Errorf("both hooks should run when all decline: %v", seen)
	}
}

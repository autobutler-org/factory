package lease_test

import (
	"errors"
	"testing"

	"github.com/autobutler-org/factory/internal/lease"
)

func ptr(s string) *string { return &s }

func TestCheck(t *testing.T) {
	tests := []struct {
		name      string
		claimedBy *string
		worker    string
		live      bool
		want      error
	}{
		{name: "live claimant", claimedBy: ptr("w1"), worker: "w1", live: true},
		{name: "expired claimant", claimedBy: ptr("w1"), worker: "w1", want: lease.ErrExpired},
		{name: "another worker", claimedBy: ptr("w1"), worker: "w2", live: true, want: lease.ErrNotClaimant},
		{name: "unclaimed", worker: "w1", want: lease.ErrNotClaimant},
		{name: "no worker named", claimedBy: ptr("w1"), live: true, want: lease.ErrNotClaimant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lease.Check(tt.claimedBy, tt.worker, tt.live); !errors.Is(got, tt.want) || (tt.want == nil && got != nil) {
				t.Fatalf("Check = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAfterFailure(t *testing.T) {
	p := lease.Policy{MaxAttempts: 3}
	tests := []struct {
		attempts  int
		retryable bool
		want      lease.Next
	}{
		{attempts: 1, retryable: true, want: lease.Retry},
		{attempts: 2, retryable: true, want: lease.Retry},
		{attempts: 3, retryable: true, want: lease.GiveUp},
		{attempts: 4, retryable: true, want: lease.GiveUp},
		{attempts: 1, retryable: false, want: lease.GiveUp},
	}
	for _, tt := range tests {
		if got := p.AfterFailure(tt.attempts, tt.retryable); got != tt.want {
			t.Errorf("AfterFailure(%d, %v) = %v, want %v", tt.attempts, tt.retryable, got, tt.want)
		}
	}
}

func TestDefaultPolicy(t *testing.T) {
	p := lease.DefaultPolicy()
	if p.Lease <= 0 || p.StuckAfter <= p.Lease || p.MaxAttempts != 3 || p.RetryDelay <= 0 {
		t.Fatalf("DefaultPolicy = %+v", p)
	}
	if err := (lease.Policy{}).Validate(); err == nil {
		t.Error("a zero policy validated")
	}
	if err := p.Validate(); err != nil {
		t.Errorf("default policy: %v", err)
	}
}

package domain

import (
	"math/rand"
	"testing"
)

func TestPriorityRank(t *testing.T) {
	known := []struct {
		name string
		rank int
	}{
		{"Highest", 1}, {"High", 2}, {"Medium", 3}, {"Low", 4}, {"Lowest", 5},
	}
	for _, tc := range known {
		if got := PriorityRank(tc.name); got != tc.rank {
			t.Fatalf("PriorityRank(%q) = %d, want %d", tc.name, got, tc.rank)
		}
	}
	if got := PriorityRank(""); got <= 5 {
		t.Fatalf("empty priority must rank below known priorities, got %d", got)
	}
	if got := PriorityRank("Showstopper"); got <= 5 {
		t.Fatalf("unknown priority must rank below known priorities, got %d", got)
	}
}

func TestAdmissionOrder_PriorityThenFifo(t *testing.T) {
	c := []Candidate{
		{QueueID: "low-old", PriorityRank: 4, FifoSeq: 1},
		{QueueID: "high-new", PriorityRank: 2, FifoSeq: 3},
		{QueueID: "high-old", PriorityRank: 2, FifoSeq: 2},
		{QueueID: "med", PriorityRank: 3, FifoSeq: 4},
	}
	AdmissionOrder(c)
	want := []string{"high-old", "high-new", "med", "low-old"}
	for i, id := range want {
		if c[i].QueueID != id {
			t.Fatalf("position %d = %q, want %q (full: %+v)", i, c[i].QueueID, id, c)
		}
	}
}

func TestGrants_CapacityPrefix(t *testing.T) {
	c := []Candidate{
		{QueueID: "a"}, {QueueID: "b"}, {QueueID: "c"}, {QueueID: "d"},
	}
	got := Grants(c, 0, MaxCodingSlots)
	if len(got) != 3 || got[0].QueueID != "a" || got[2].QueueID != "c" {
		t.Fatalf("want first 3 granted, got %+v", got)
	}
	if Grants(c, 3, MaxCodingSlots) != nil {
		t.Fatal("full capacity must grant nothing")
	}
	if got := Grants(c, 2, MaxCodingSlots); len(got) != 1 || got[0].QueueID != "a" {
		t.Fatalf("one free slot grants head only, got %+v", got)
	}
}

// The AC-02 invariant stated as a property: for every candidate ordering and
// every starting hold count, granted + held never exceeds MaxCodingSlots.
func TestGrants_NeverExceedsBudget(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		n := r.Intn(10)
		c := make([]Candidate, n)
		for j := range c {
			c[j] = Candidate{QueueID: string(rune('a' + j)), PriorityRank: r.Intn(6) + 1, FifoSeq: int64(j)}
		}
		AdmissionOrder(c)
		held := r.Intn(MaxCodingSlots + 1)
		got := Grants(c, held, MaxCodingSlots)
		if held+len(got) > MaxCodingSlots {
			t.Fatalf("held %d + granted %d > %d", held, len(got), MaxCodingSlots)
		}
		if want := MaxCodingSlots - held; len(got) != min(want, len(c)) {
			t.Fatalf("held %d granted %d, want %d", held, len(got), min(want, len(c)))
		}
		// Order preserved under the grant.
		for j := 1; j < len(got); j++ {
			if Less(got[j], got[j-1]) {
				t.Fatalf("grant order violated at %d: %+v", j, got)
			}
		}
	}
}

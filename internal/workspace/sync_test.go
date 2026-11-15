package workspace

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type fakeHub struct {
	pending  []string
	observed map[string]string
	calls    int
}

func (h *fakeHub) exchange(_ context.Context, obs []Observation) ([]string, error) {
	h.calls++
	for _, o := range obs {
		if _, ok := h.observed[o.Path]; !ok {
			h.observed[o.Path] = o.Repo
		}
	}
	var out []string
	for _, p := range h.pending {
		if _, ok := h.observed[p]; !ok {
			out = append(out, p)
		}
		if len(out) == 2 {
			break
		}
	}
	return out, nil
}

func TestSyncObservesEveryPendingPath(t *testing.T) {
	hub := &fakeHub{pending: []string{"/a", "/b", "/c", "/d", "/e"}, observed: map[string]string{}}
	res, err := Sync(context.Background(), hub.exchange, func(p string) (Observation, error) {
		return Observation{Path: p, Repo: "github.com/x" + p}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Observed != 5 || len(hub.observed) != 5 || hub.observed["/e"] != "github.com/x/e" {
		t.Fatalf("result %+v, hub %v", res, hub.observed)
	}
}

func TestSyncStopsWhenOnlyFailedPathsRemain(t *testing.T) {
	hub := &fakeHub{pending: []string{"/locked", "/a", "/b"}, observed: map[string]string{}}
	res, err := Sync(context.Background(), hub.exchange, func(p string) (Observation, error) {
		if p == "/locked" {
			return Observation{}, errors.New("permission denied")
		}
		return Observation{Path: p}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Observed != 2 || len(res.Failures) != 1 || hub.calls > 4 {
		t.Fatalf("result %+v after %d calls", res, hub.calls)
	}
	if _, ok := hub.observed["/locked"]; ok {
		t.Fatal("a failed path was observed")
	}
}

func TestSyncSendsTheLastRoundAtTheRoundLimit(t *testing.T) {
	var sent []string
	next := 0
	exchange := func(_ context.Context, obs []Observation) ([]string, error) {
		for _, o := range obs {
			sent = append(sent, o.Path)
		}
		next++
		return []string{string(rune('A'+next%26)) + string(rune('0'+next/26))}, nil
	}
	res, err := Sync(context.Background(), exchange, func(p string) (Observation, error) { return Observation{Path: p}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Observed != MaxRounds || len(sent) != MaxRounds || !slices.Contains(sent, "A1") {
		t.Fatalf("observed %d, sent %d", res.Observed, len(sent))
	}
}

func TestSyncReturnsExchangeErrors(t *testing.T) {
	want := errors.New("hub gone")
	_, err := Sync(context.Background(), func(context.Context, []Observation) ([]string, error) { return nil, want },
		func(string) (Observation, error) { return Observation{}, nil })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

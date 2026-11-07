package tenant

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/labels"
)

func TestExpiredRuns(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	past := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	l := func(run, exp string) map[string]string {
		m := map[string]string{labels.RunID: run}
		if exp != "-" {
			m[labels.ExpiresAt] = exp
		}
		return m
	}
	res := []Resource{
		// live test tenant of another engineer: kept
		{Kind: "server", Labels: l("rlive001", future)},
		{Kind: "volume", Labels: l("rlive001", future)},
		// expired tenant: deleted
		{Kind: "server", Labels: l("rexp0001", past)},
		{Kind: "dns_record", Labels: l("rexp0001", past)},
		// mixed expiry: one expired resource is enough
		{Kind: "server", Labels: l("rmixed01", future)},
		{Kind: "volume", Labels: l("rmixed01", past)},
		// real install: never deleted, even with a volume carrying a broken expiry
		{Kind: "server", Labels: l("rinstall", labels.Never)},
		{Kind: "volume", Labels: l("rinstall", "-62135596800")},
		// live tenant with an old stray ACME challenge record: kept
		{Kind: "server", Labels: l("rlive002", future)},
		{Kind: "dns_record", Labels: l("rlive002", "0"), Inferred: true},
		// only a stray ACME challenge record left: deleted
		{Kind: "dns_record", Labels: l("rstray01", "0"), Inferred: true},
		// young stray ACME challenge record: kept
		{Kind: "dns_record", Labels: l("rstray02", future), Inferred: true},
		// half-labelled leftover of a crashed run: deleted
		{Kind: "ssh_key", Labels: l("rcrash01", "-")},
		// half-labelled resource in a live run: kept
		{Kind: "server", Labels: l("rlive003", future)},
		{Kind: "ssh_key", Labels: l("rlive003", "garbage")},
		// not a run id: ignored
		{Kind: "server", Labels: l("someone-else", past)},
	}
	runs, kept := ExpiredRuns(res, now)
	if want := []string{"rcrash01", "rexp0001", "rmixed01", "rstray01"}; !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %v, want %v", runs, want)
	}
	var keptIDs []string
	for _, k := range kept {
		keptIDs = append(keptIDs, k.RunID)
	}
	if want := []string{"rinstall", "rlive002", "rlive003"}; !reflect.DeepEqual(keptIDs, want) {
		t.Errorf("kept = %v, want %v", keptIDs, want)
	}
}

func TestParamsNeverExpires(t *testing.T) {
	p := &Provisioner{}
	if got := p.Params(&State{RunID: "rabcdefg"}).ExpiresAt; got != labels.Never {
		t.Errorf("ExpiresAt for an install without expiry = %q, want %q", got, labels.Never)
	}
	exp := time.Unix(1_800_000_000, 0)
	if got := p.Params(&State{RunID: "rabcdefg", ExpiresAt: exp}).ExpiresAt; got != "1800000000" {
		t.Errorf("ExpiresAt = %q", got)
	}
}

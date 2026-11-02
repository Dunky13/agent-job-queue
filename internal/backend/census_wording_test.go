package backend

import (
	"errors"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/census"
)

func TestCensusCallersKeepTheirEstablishedMessages(t *testing.T) {
	observer := census.Observer{PID: 999, PGID: 900}
	const observerRow = "999 900 501 S Mon Nov 2 18:22:25 2026\n"
	listings := []struct {
		output string
		kind   error
		pick   func(census.Wording) string
	}{
		{observerRow + "invalid\n", census.ErrTruncated, func(w census.Wording) string { return w.Truncated }},
		{observerRow + "abc 900 501 S Mon Nov 2 18:22:25 2026\n", census.ErrInvalid, func(w census.Wording) string { return w.Invalid }},
		{observerRow + observerRow, census.ErrAmbiguous, func(w census.Wording) string { return w.Ambiguous }},
		{"101 400 501 S Mon Nov 2 18:22:25 2026\n", census.ErrIncomplete, func(w census.Wording) string { return w.Incomplete }},
	}
	for _, site := range []struct {
		name    string
		wording census.Wording
		want    [4]string
	}{
		{"ProcessGroupMembers", processIdentityWording, [4]string{
			"Invalid process identity listing; shared slot retained", "Invalid process identity listing; shared slot retained",
			"Ambiguous process identity listing; shared slot retained", "Incomplete process identity listing; shared slot retained"}},
		{"processGroupStates", processStateWording, [4]string{
			"Invalid process state listing; shared slot retained", "Invalid process state listing; shared slot retained",
			"Ambiguous process state listing; shared slot retained", "Incomplete process state listing; shared slot retained"}},
		{"resource census", resourceCensusWording, [4]string{
			"Process resource identity listing is invalid", "Process resource identity listing is invalid",
			"Process resource identity listing is inconsistent", "Process resource identity listing is incomplete"}},
		{"handover", queueHandoverCensusWording, [4]string{
			"Incomplete handover process snapshot", "Invalid handover process snapshot",
			"Ambiguous handover process snapshot", "Incomplete handover process snapshot"}},
		{"lease owner", leaseOwnerCensusWording, [4]string{
			"Incomplete lease owner process listing", "Invalid lease owner process listing",
			"Ambiguous lease owner process listing", "Incomplete lease owner process listing"}},
	} {
		for i, listing := range listings {
			_, err := census.Parse(listing.output, observer)
			if !errors.Is(err, listing.kind) {
				t.Fatalf("listing %q rejected as %v, want %v", listing.output, err, listing.kind)
			}
			if got := site.wording.Of(err).Error(); got != site.want[i] {
				t.Errorf("%s reports %q for %v, want %q", site.name, got, listing.kind, site.want[i])
			}
		}
	}
}

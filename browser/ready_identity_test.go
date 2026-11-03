package browser_test

import (
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/browser"
)

func TestReadyPreservesEachObjectAndItsExactMetadataOwner(t *testing.T) {
	tests := []struct {
		name   string
		change func(*browser.ReadyReport)
	}{
		{"shared object identity", func(report *browser.ReadyReport) {
			report.Objects[1].ObjectID = report.Objects[0].ObjectID
			report.Contributions = report.Contributions[:1]
		}},
		{"relabelled metadata owner", func(report *browser.ReadyReport) {
			report.Contributions[0].Object.Export = report.Objects[1].Export
		}},
		{"orphan metadata", func(report *browser.ReadyReport) {
			orphan := report.Contributions[0]
			orphan.Object.ObjectID = "orphan"
			report.Contributions = append(report.Contributions, orphan)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
			plan, activated := e.beginActivation("publisher")
			report := e.readyReport(plan)
			test.change(&report)
			result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report)
			if err != nil || result.Decision != browser.Rejected {
				t.Fatalf("inconsistent prepared identity was accepted: %+v %v", result, err)
			}
			if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, e.readyReport(plan)); err != nil || result.Decision != browser.Accepted {
				t.Fatalf("valid report could not finish original preparation: %+v %v", result, err)
			}
			if err := <-activated; err != nil {
				t.Fatal(err)
			}
		})
	}
}

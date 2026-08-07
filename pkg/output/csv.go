package output

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

type csvRenderer struct{}

func (csvRenderer) Format() string { return "csv" }

var csvHeader = []string{
	"resource", "rule", "monthly_waste_usd", "severity", "confidence",
	"kind", "project", "location", "resource_id", "evidence",
}

func (csvRenderer) Render(w io.Writer, r Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, f := range r.Findings {
		row := []string{
			f.Resource,
			f.RuleID,
			fmt.Sprintf("%.2f", f.MonthlyWasteUSD),
			string(f.Severity),
			fmt.Sprintf("%.2f", f.Confidence),
			string(f.Kind),
			f.Project,
			f.Location,
			string(f.ResourceID),
			flattenEvidence(f.Evidence),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func flattenEvidence(ev []rules.Evidence) string {
	if len(ev) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ev))
	for _, e := range ev {
		parts = append(parts, e.Key+"="+e.Value)
	}
	return strings.Join(parts, "; ")
}

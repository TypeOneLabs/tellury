package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/TypeOneLabs/tellury/pkg/rules"
)

func newRulesCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Inspect the rule catalogue",
	}
	cmd.AddCommand(newRulesListCmd(), newRulesExplainCmd())
	return cmd
}

func newRulesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every registered rule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tPROVIDER\tSERVICE\tSEVERITY\tMETRICS\tTITLE")
			for _, r := range rules.List() {
				m := r.Meta()
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
					m.ID, m.Provider, m.Service, m.Severity, len(m.RequiredMetrics), m.Title)
			}
			return tw.Flush()
		},
	}
}

func newRulesExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain RULE_ID",
		Short: "Print a rule's full declaration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ok := rules.Get(args[0])
			if !ok {
				return newUsageError(fmt.Errorf("unknown rule %q (see `tellury rules list`)", args[0]))
			}
			m := r.Meta()
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s (%s/%s)\n\n", m.ID, m.Provider, m.Service)
			fmt.Fprintf(w, "  title:        %s\n", m.Title)
			fmt.Fprintf(w, "  severity:     %s\n", m.Severity)
			fmt.Fprintf(w, "  origin:       %s\n", m.Origin)
			fmt.Fprintf(w, "  asset types:  %s\n", joinOrNone(m.RequiredAssetTypes))
			fmt.Fprintf(w, "  metrics:      %s\n", joinOrNone(m.RequiredMetrics))
			fmt.Fprintf(w, "  remediation:  %s\n", m.Remediation)
			if m.Description != "" {
				fmt.Fprintf(w, "\n%s\n", m.Description)
			}
			return nil
		},
	}
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

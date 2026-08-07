package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TypeOneLabs/tellury/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Long())
			return err
		},
	}
}

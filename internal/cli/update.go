package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/andreujuanc/atelier/internal/update"
	"github.com/andreujuanc/atelier/internal/version"
)

func (c *cli) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print atl's version (the release tag, or dev for a local build)",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(c.stdout, version.String())
			return nil
		},
	}
}

func (c *cli) updateCmd() *cobra.Command {
	var check, force bool
	cmd := &cobra.Command{
		Use:   "update [--check] [--force]",
		Short: "replace atl with its latest release (signed with the release key, checksum checked)",
		Long: "Replaces the running atl binary with the latest release from GitHub, once the\n" +
			"release's SHA256SUMS.sig checks out with the release key built into atl and the\n" +
			"binary's checksum is the signed one. Running sessions and gates keep the old binary\n" +
			"until they restart; the TUI restarts itself when you update from it (U).\n\n" +
			"A local build (version dev) is only replaced with --force.",
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := update.New()
			cur := version.Current()
			rel, err := u.Latest(cmd.Context())
			if err != nil {
				return err
			}
			newer := version.Newer(cur, rel.Tag)
			if check {
				switch {
				case newer:
					fmt.Fprintf(c.stdout, "atl %s → %s available: atl update\n", cur, rel.Tag)
				case cur == version.Dev:
					fmt.Fprintf(c.stdout, "atl %s (a local build); latest release %s\n", version.String(), rel.Tag)
				default:
					fmt.Fprintf(c.stdout, "atl %s is up to date\n", cur)
				}
				return nil
			}
			if !newer && !force {
				if cur == version.Dev {
					return fmt.Errorf("this atl is a local build (%s); atl update --force replaces it with release %s", version.String(), rel.Tag)
				}
				fmt.Fprintf(c.stdout, "atl %s is up to date\n", cur)
				return nil
			}
			target, err := update.Target()
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "downloading atl %s …\n", rel.Tag)
			if err := u.Install(cmd.Context(), rel, target); err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "updated %s: %s → %s (sessions and gates use it when they restart)\n", target, cur, rel.Tag)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	cmd.Flags().BoolVar(&force, "force", false, "install the latest release even over a local build or the same version")
	return cmd
}

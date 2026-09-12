package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const appName = "topdata-telemetry"

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     appName,
	Short:   "Read-only Shopware 6 fleet telemetry agent",
	Version: version,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

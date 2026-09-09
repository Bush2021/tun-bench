package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/spf13/cobra"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := newCommand().ExecuteContext(ctx)
	if err != nil {
		log.Fatal(err)
	}
}

func newCommand() *cobra.Command {
	rootCommand := &cobra.Command{
		Use:           "tun-bench",
		Short:         "Benchmark TUN implementations",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	var outputPath string
	var matrixName string
	var measurementType string
	var overwrite bool
	runCommand := &cobra.Command{
		Use:   "run configuration.yaml",
		Short: "Run or resume a matrix and save its measurements as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			configuration, cases, err := readConfiguration(args[0], matrixName, measurementType)
			if err != nil {
				return err
			}
			destination := outputPath
			if destination == "" {
				destination = strings.TrimSuffix(args[0], filepath.Ext(args[0])) + ".json"
			}
			destination, err = filepath.Abs(destination)
			if err != nil {
				return err
			}
			source, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if destination == source {
				return E.New("result path must differ from configuration path")
			}
			runner := environmentMatrix{
				configuration: configuration,
				cases:         cases,
				destination:   destination, failFast: configuration.FailFast, overwrite: overwrite,
			}
			return runner.run(command.Context())
		},
	}
	runCommand.Flags().StringVarP(&outputPath, "output", "o", "", "Result JSON path (defaults to the configuration name with .json)")
	runCommand.Flags().StringVar(&matrixName, "name", "", "Run only the matrix with this exact name")
	runCommand.Flags().StringVar(&measurementType, "type", "", "Run only throughput or memory matrices")
	runCommand.Flags().BoolVar(&overwrite, "overwrite", false, "Start a fresh run, replacing existing results")
	common.Must(runCommand.MarkFlagFilename("output", "json"))
	formatCommand := &cobra.Command{
		Use:   "format results.json",
		Short: "Render saved measurements as text",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			input := command.InOrStdin()
			if args[0] != "-" {
				file, err := openResult(args[0])
				if err != nil {
					return err
				}
				defer file.Close()
				input = file
			}
			report, err := readReport(input)
			if err != nil {
				return err
			}
			return report.writeSummary(command.OutOrStdout())
		},
	}
	rootCommand.AddCommand(runCommand, formatCommand, &cobra.Command{
		Use: "internal-worker [bundle.tar]", Hidden: true, Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			var bundlePath string
			if len(args) > 0 {
				bundlePath = args[0]
			}
			return runWorker(command.Context(), command.InOrStdin(), command.OutOrStdout(), bundlePath)
		},
	}, &cobra.Command{
		Use:                "internal-exec CPUs executable [args...]",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(2),
		RunE:               func(command *cobra.Command, args []string) error { return execChild(args) },
	}, &cobra.Command{
		Use:    "internal-interrupt PID",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE:   func(command *cobra.Command, args []string) error { return execInterrupt(args[0]) },
	})
	return rootCommand
}

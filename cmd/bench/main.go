package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "validate":
		configPath, err := parseConfigFlag("validate", arguments[1:])
		if err != nil {
			return err
		}
		loaded, err := experiment.LoadConfig(configPath)
		if err != nil {
			return err
		}
		bundle, err := experiment.Validate(context.Background(), loaded)
		if err != nil {
			return err
		}
		fmt.Printf("validated %d cases; workload=%s result=%s bundle=%s\n",
			len(bundle.ValidatedCases), bundle.WorkloadHash, bundle.ResultDigest, loaded.Config.Output.ValidationBundle)
		return nil

	case "run":
		configPath, err := parseConfigFlag("run", arguments[1:])
		if err != nil {
			return err
		}
		loaded, err := experiment.LoadConfig(configPath)
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		invoke := subprocessInvoker(executable, configPath)
		if err := experiment.RunMatrix(context.Background(), loaded, invoke); err != nil {
			return err
		}
		fmt.Printf("completed matrix; records=%s traces=%s\n", loaded.Config.Output.RunRecords, loaded.Config.Output.ActionTraces)
		return nil

	case "_worker":
		return runWorker(arguments[1:])

	default:
		return usageError()
	}
}

func parseConfigFlag(command string, arguments []string) (string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "path to experiment-matrix-v3 JSON")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return "", fmt.Errorf("usage: bench %s -config <path>", command)
	}
	return *configPath, nil
}

func subprocessInvoker(executable, configPath string) experiment.WorkerInvoker {
	return func(ctx context.Context, request experiment.WorkerRequest) (experiment.WorkerResponse, error) {
		command := exec.CommandContext(
			ctx,
			executable,
			"_worker",
			"-config", configPath,
			"-case", request.CaseID,
			"-phase", request.Phase,
			"-round", strconv.Itoa(request.Round),
			"-order", strconv.Itoa(request.Order),
		)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return experiment.WorkerResponse{}, fmt.Errorf("worker timeout: %w", ctx.Err())
			}
			return experiment.WorkerResponse{}, fmt.Errorf("worker failed: %w: %s", err, stderr.String())
		}
		var response experiment.WorkerResponse
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			return experiment.WorkerResponse{}, fmt.Errorf("decode worker response: %w", err)
		}
		return response, nil
	}
}

func runWorker(arguments []string) error {
	flags := flag.NewFlagSet("_worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "experiment config")
	caseID := flags.String("case", "", "case id")
	phase := flags.String("phase", "", "warmup or measurement")
	round := flags.Int("round", -1, "round")
	order := flags.Int("order", -1, "order")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *configPath == "" || *caseID == "" || (*phase != "warmup" && *phase != "measurement") || *round < 0 || *order < 0 || flags.NArg() != 0 {
		return errors.New("invalid worker arguments")
	}
	loaded, err := experiment.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loaded.Timeout)
	defer cancel()
	response, err := experiment.RunWorker(ctx, loaded, experiment.WorkerRequest{
		CaseID: *caseID,
		Phase:  *phase,
		Round:  *round,
		Order:  *order,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(response)
}

func usageError() error {
	return errors.New("usage: bench <validate|run> -config <path>")
}

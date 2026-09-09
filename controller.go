package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
)

type environmentMatrix struct {
	configuration runConfiguration
	cases         []benchmarkOptions
	destination   string
	failFast      bool
	overwrite     bool
}

type environmentJob struct {
	index  int
	cases  []benchmarkOptions
	runner string
}

func (m *environmentMatrix) run(ctx context.Context) (returnErr error) {
	report := &resultReport{StartedAt: time.Now().UTC()}
	if !m.overwrite {
		file, err := openResult(m.destination)
		if err == nil {
			report, err = readReport(file)
			err = E.Errors(err, file.Close())
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return E.Cause(err, "resume results")
		}
	}
	names := common.Uniq(common.Map(m.cases, func(options benchmarkOptions) string { return options.environmentName }))
	var jobs []environmentJob
	for _, name := range names {
		target := m.configuration.Environments[name]
		index := slices.IndexFunc(report.Environments, func(entry environmentResult) bool { return entry.Name == name })
		if index < 0 {
			index = len(report.Environments)
			report.Environments = append(report.Environments, environmentResult{Name: name, Configuration: target})
		} else {
			previousConfiguration := report.Environments[index].Configuration
			previousConfiguration.Device = target.Device
			if previousConfiguration != target {
				return E.New("environment ", name, " differs from saved results; use a new name, another output path or --overwrite")
			}
			report.Environments[index].Configuration = target
		}
		cases := common.Filter(m.cases, func(options benchmarkOptions) bool { return options.environmentName == name })
		previous := report.Environments[index].Report
		if previous != nil {
			for i := range cases {
				options := &cases[i]
				implementation, loaded := previous.Implementations[options.Implementation]
				if !loaded {
					continue
				}
				if options.version == "latest" && implementation.Version != "" {
					options.version = implementation.Version
				}
				if options.software != implementation.Type || options.version != implementation.Version || options.sourcePackage != implementation.Package {
					return E.New("implementation ", options.Implementation, " in environment ", name, " differs from saved results; use a new name, another output path or --overwrite")
				}
			}
		}
		var runnable []benchmarkOptions
		for _, options := range cases {
			if target.OS != "windows" || options.software != "hev-socks5-tunnel" {
				runnable = append(runnable, options)
				continue
			}
			if previous == nil {
				previous = &benchmarkReport{
					StartedAt: time.Now().UTC(), Protocol: currentProtocol,
					Environment:     resultEnvironment{OS: target.OS, Arch: target.Arch, Metric: metricCPU},
					Implementations: make(map[string]implementationConfiguration),
				}
				report.Environments[index].Report = previous
			}
			previous.Implementations[options.Implementation] = implementationConfiguration{
				Type: options.software, Package: options.sourcePackage, Version: options.version,
			}
			recorded := caseResult{
				caseConfiguration: options.caseConfiguration, Queues: 1, Status: "bug",
				Bug: &caseBug{Type: "windows-hev", Detail: "Known Windows HEV forwarding and shutdown hangs; execution disabled."},
			}
			if options.Type == "memory" {
				recorded.Memory = &memoryResult{
					ConnectionStep: memoryConnectionStep, MaxConnections: memoryMaxConnections,
					Protocol: currentMemoryProtocol, Metric: memoryMetric,
				}
			}
			caseIndex := slices.IndexFunc(previous.Cases, func(saved caseResult) bool {
				return saved.caseConfiguration == options.caseConfiguration
			})
			if caseIndex < 0 {
				previous.Cases = append(previous.Cases, recorded)
			} else {
				previous.Cases[caseIndex] = recorded
			}
			log.Info(options, ": bug (windows-hev); execution skipped")
		}
		if len(runnable) != len(cases) && completedCases(runnable, previous) == len(runnable) {
			finished := time.Now().UTC()
			previous.FinishedAt, previous.Error = &finished, ""
			report.Environments[index].Error = ""
		}
		jobs = append(jobs, environmentJob{index: index, cases: runnable})
	}
	report.FinishedAt, report.Error = nil, ""
	defer func() {
		finished := time.Now().UTC()
		report.FinishedAt = &finished
		if returnErr != nil {
			report.Error = returnErr.Error()
		}
		returnErr = E.Append(returnErr, report.save(m.destination), func(saveErr error) error { return E.Cause(saveErr, "save results") })
	}()
	err := report.save(m.destination)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		log.Warn("No supported benchmark cases to run")
		return nil
	}
	cache, err := cacheDirectory()
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp(cache, "artifacts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	preparer, err := newArtifactPreparer(cache, directory)
	if err != nil {
		return err
	}
	defer preparer.client.client.CloseIdleConnections()
	for i := range jobs {
		job := &jobs[i]
		entry := &report.Environments[job.index]
		if completedCases(job.cases, entry.Report) == len(job.cases) {
			continue
		}
		job.cases, err = preparer.prepare(ctx, job.cases, entry.Configuration, entry.Report)
		if err == nil && len(job.cases) > 0 && entry.Configuration.Type != "local" {
			job.runner, err = preparer.runner(ctx, entry.Configuration)
		}
		if err != nil {
			entry.Error = err.Error()
			return E.Cause(err, "environment ", entry.Name)
		}
		entry.Error = ""
	}
	formatRun := benchmarkLog(common.FlatMap(jobs, func(job environmentJob) []benchmarkOptions { return job.cases }))
	jobsByDevice := make(map[string][]environmentJob)
	progress := 0
	total := 0
	for _, job := range jobs {
		if len(job.cases) == 0 {
			continue
		}
		entry := report.Environments[job.index]
		total += len(job.cases)
		progress += completedCases(job.cases, entry.Report)
		device := entry.Configuration.Device
		jobsByDevice[device] = append(jobsByDevice[device], job)
	}
	log.Info("Progress: ", progress, "/", total, " completed; ", total-progress, " to run")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var access sync.Mutex
	var workers sync.WaitGroup
	for _, deviceJobs := range jobsByDevice {
		workers.Go(func() {
			for _, job := range deviceJobs {
				access.Lock()
				if runCtx.Err() != nil {
					access.Unlock()
					return
				}
				entry := &report.Environments[job.index]
				if completedCases(job.cases, entry.Report) == len(job.cases) {
					access.Unlock()
					log.Info("Environment ", entry.Name, ": all selected cases completed")
					continue
				}
				entry.Error = ""
				access.Unlock()
				log.Info("Environment ", entry.Name, " (", entry.Configuration.OS, "/", entry.Configuration.Arch, ")")
				var recordErr error
				save := func(updated *benchmarkReport, started *caseResult) error {
					access.Lock()
					defer access.Unlock()
					entry.Report = updated
					recordErr = E.Errors(recordErr, report.save(m.destination))
					if recordErr != nil {
						cancel()
					} else if started != nil {
						implementation := updated.Implementations[started.Implementation]
						options := benchmarkOptions{
							caseConfiguration: started.caseConfiguration,
							environmentName:   entry.Name, software: implementation.Type,
							version: implementation.Version, queues: started.Queues,
						}
						label := formatRun(options)
						if label != "" {
							label = " " + label
						}
						progress++
						log.Info("[", progress, "/", total, "]", label)
					}
					return recordErr
				}
				var runErr error
				if entry.Configuration.Type == "local" {
					runErr = runLocal(runCtx, directory, entry.Configuration, job.cases, entry.Report, m.failFast, save)
				} else {
					runErr = runRemote(runCtx, entry.Configuration, job.runner, job.cases, entry.Report, m.failFast, save)
				}
				access.Lock()
				if runErr != nil {
					entry.Error = runErr.Error()
					returnErr = E.Errors(returnErr, E.Cause(runErr, "environment ", entry.Name))
				}
				saveErr := report.save(m.destination)
				returnErr = E.Errors(returnErr, saveErr)
				if recordErr != nil || saveErr != nil || runErr != nil && m.failFast {
					cancel()
				}
				access.Unlock()
			}
		})
	}
	workers.Wait()
	return E.Errors(returnErr, ctx.Err())
}

func completedCases(cases []benchmarkOptions, report *benchmarkReport) int {
	if report == nil {
		return 0
	}
	return len(common.Filter(cases, func(options benchmarkOptions) bool {
		options.resolveQueues(environment{workers: report.Environment.TunnelWorkers})
		return slices.ContainsFunc(report.Cases, func(recorded caseResult) bool {
			return (recorded.Status == "passed" || recorded.Status == "bug") &&
				recorded.matches(options)
		})
	}))
}

func runLocal(ctx context.Context, directory string, target environmentConfiguration, cases []benchmarkOptions, report *benchmarkReport, failFast bool, save func(*benchmarkReport, *caseResult) error) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	bundleDirectory, err := os.MkdirTemp(directory, "local-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bundleDirectory)
	bundlePath := filepath.Join(bundleDirectory, "bundle.tar")
	err = writeWorkerBundle(bundlePath, cases, target, report, failFast)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, "internal-worker")
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		log.Info("Start local test worker with sudo")
		command = exec.CommandContext(ctx, "sudo", "-H", "--", executable, "internal-worker")
	}
	return runWorkerProcess(ctx, command, bundlePath, save)
}

const windowsCleanupScript = "for ($cleanupAttempt = 0; $cleanupAttempt -le 20; $cleanupAttempt++) { " +
	"try { if (Test-Path -LiteralPath $directory) { Remove-Item -LiteralPath $directory -Recurse -Force -ErrorAction Stop }; break } " +
	"catch { if ($cleanupAttempt -eq 20) { throw }; Start-Sleep -Milliseconds 250 } }"

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (e environmentConfiguration) command(ctx context.Context, script string) *exec.Cmd {
	if e.Type == "orbstack" {
		args := []string{"-u", e.User}
		if e.Name != "" {
			args = append(args, "-m", e.Name)
		}
		return exec.CommandContext(ctx, "orb", append(args, "sh", "-c", script)...)
	}
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	if e.Port != 0 {
		args = append(args, "-p", strconv.Itoa(e.Port))
	}
	if e.User != "" {
		args = append(args, "-l", e.User)
	}
	if e.IdentityFile != "" {
		args = append(args, "-i", e.IdentityFile)
	}
	if e.OS == "windows" {
		script = "$ErrorActionPreference = 'Stop'; " + script
	} else {
		script = "sh -c " + shellQuote(script)
	}
	return exec.CommandContext(ctx, "ssh", append(args, e.Host, script)...)
}

func runRemote(ctx context.Context, target environmentConfiguration, executable string, cases []benchmarkOptions, report *benchmarkReport, failFast bool, save func(*benchmarkReport, *caseResult) error) (returnErr error) {
	name := "tun-bench-" + rand.Text()
	persistent := `"${XDG_CACHE_HOME:-$HOME/.cache}/tun-bench"`
	if target.OS == "darwin" {
		persistent = `"$HOME/Library/Caches/tun-bench"`
	}
	uploadScript := `umask 077; for base in "${TMPDIR:-/tmp}" ` + persistent + `; do directory="$base/` + name + `"; if mkdir -p "$base" 2>/dev/null && mkdir -m 700 "$directory" 2>/dev/null; then printf '%s\n' "$directory"; cat > "$directory/tun-bench" && chmod 700 "$directory/tun-bench"; exit $?; fi; done; echo 'cannot create remote cache directory' >&2; exit 1`
	if target.OS == "windows" {
		uploadScript = `$bases = @([IO.Path]::GetTempPath(), [IO.Path]::Combine([Environment]::GetFolderPath('LocalApplicationData'), 'tun-bench')); $directory = $null; foreach ($base in $bases) { $candidate = [IO.Path]::Combine($base, '` + name + `'); try { New-Item -ItemType Directory -Path $candidate -ErrorAction Stop | Out-Null; $directory = $candidate; break } catch {} }; if (!$directory) { throw 'cannot create remote cache directory' }; [Console]::Out.WriteLine($directory)`
	}
	upload := target.command(ctx, uploadScript)
	if target.OS != "windows" {
		content, openErr := os.Open(executable)
		if openErr != nil {
			return openErr
		}
		defer content.Close()
		upload.Stdin = content
	}
	var uploadOutput processOutput
	var uploadPath bytes.Buffer
	captureCommandOutput(upload, &uploadPath, os.Stderr, &uploadOutput)
	err := upload.Run()
	remoteDirectory := strings.TrimSpace(uploadPath.String())
	if remoteDirectory == "" || strings.ContainsAny(remoteDirectory, "\r\n\x00") {
		return E.Errors(commandError(upload, err, &uploadOutput), E.New("upload did not return a remote directory"))
	}
	remoteExecutable := remoteDirectory + "/tun-bench"
	cleanupScript := "rm -rf -- " + shellQuote(remoteDirectory)
	runScript := "trap " + shellQuote(cleanupScript) + " EXIT; "
	if target.User != "root" {
		runScript += "if [ \"$(id -u)\" -eq 0 ]; then " + shellQuote(remoteExecutable) + " internal-worker; else sudo -n -- " + shellQuote(remoteExecutable) + " internal-worker; fi"
	} else {
		runScript += shellQuote(remoteExecutable) + " internal-worker"
	}
	if target.OS == "windows" {
		setup := "$directory = '" + strings.ReplaceAll(remoteDirectory, "'", "''") + "'; $executable = [IO.Path]::Combine($directory, '" + filepath.Base(executable) + "'); "
		cleanupScript = setup + windowsCleanupScript
		runScript = setup + "try { & $executable internal-worker ([IO.Path]::Combine($directory, 'bundle.tar')); $code = $LASTEXITCODE } finally { " + windowsCleanupScript + " }; exit $code"
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
		defer cancel()
		command := target.command(cleanupCtx, cleanupScript)
		var diagnostic processOutput
		captureCommandOutput(command, nil, nil, &diagnostic)
		cleanupErr := command.Run()
		returnErr = E.Append(returnErr, commandError(command, cleanupErr, &diagnostic), func(removeErr error) error { return E.Cause(removeErr, "remove remote runner") })
	}()
	if err != nil {
		return commandError(upload, err, &uploadOutput)
	}
	bundleDirectory, err := os.MkdirTemp(filepath.Dir(executable), "bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bundleDirectory)
	bundlePath := filepath.Join(bundleDirectory, "bundle.tar")
	err = writeWorkerBundle(bundlePath, cases, target, report, failFast)
	if err != nil {
		return err
	}
	if target.OS == "windows" {
		args := []string{"-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
		if target.Port != 0 {
			args = append(args, "-P", strconv.Itoa(target.Port))
		}
		if target.User != "" {
			args = append(args, "-o", "User="+target.User)
		}
		if target.IdentityFile != "" {
			args = append(args, "-i", target.IdentityFile)
		}
		transfer := exec.CommandContext(ctx, "scp", append(args, executable, bundlePath, target.Host+":"+strings.ReplaceAll(remoteDirectory, "\\", "/")+"/")...)
		var transferOutput processOutput
		captureCommandOutput(transfer, nil, os.Stderr, &transferOutput)
		err = transfer.Run()
		if err != nil {
			return commandError(transfer, err, &transferOutput)
		}
	}
	if target.OS == "windows" {
		bundlePath = ""
	}
	return runWorkerProcess(ctx, target.command(ctx, runScript), bundlePath, save)
}

const workerCleanupTimeout = 30 * time.Second

func runWorkerProcess(ctx context.Context, command *exec.Cmd, bundlePath string, save func(*benchmarkReport, *caseResult) error) error {
	var bundle *os.File
	var size int64
	var err error
	if bundlePath != "" {
		bundle, err = os.Open(bundlePath)
		if err != nil {
			return err
		}
		defer bundle.Close()
		info, statErr := bundle.Stat()
		if statErr != nil {
			return statErr
		}
		size = info.Size()
	}
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var diagnostic processOutput
	command.Stderr = io.MultiWriter(os.Stderr, &diagnostic)
	command.Cancel = func() error {
		go func() {
			_, _ = io.WriteString(input, "cancel\n")
			_ = input.Close()
		}()
		return nil
	}
	command.WaitDelay = workerCleanupTimeout
	err = command.Start()
	if err != nil {
		return err
	}
	sent := make(chan error, 1)
	go func() {
		var sendErr error
		if bundle != nil {
			sendErr = binary.Write(input, binary.BigEndian, uint64(size))
			if sendErr == nil {
				_, sendErr = io.Copy(input, bundle)
			}
			if sendErr != nil {
				input.Close()
			}
		}
		sent <- sendErr
	}()
	decoder := json.NewDecoder(output)
	received := false
	for {
		var updated workerUpdate
		decodeErr := decoder.Decode(&updated)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			err = E.Cause(decodeErr, "receive worker results")
			_ = command.Cancel()
			break
		}
		err = save(updated.Report, updated.Started)
		if err != nil {
			_ = command.Cancel()
			break
		}
		received = true
	}
	if err != nil {
		timer := time.AfterFunc(workerCleanupTimeout, func() { _ = command.Process.Kill() })
		_, _ = io.Copy(io.Discard, output)
		timer.Stop()
	}
	waitErr := command.Wait()
	input.Close()
	sendErr := <-sent
	if err == nil && waitErr == nil && !received {
		err = E.New("worker returned no results")
	}
	return E.Errors(err, sendErr, commandError(command, waitErr, &diagnostic), ctx.Err())
}

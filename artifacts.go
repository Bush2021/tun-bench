package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/google/go-github/v91/github"
)

type artifactPreparer struct {
	client      releaseClient
	directory   string
	binaries    map[[4]string]preparedBinary
	unsupported map[[4]string]error
	runners     map[[2]string]string
	versions    map[string]string
}

type preparedBinary struct{ path, version string }

func (p *artifactPreparer) prepare(ctx context.Context, cases []benchmarkOptions, target environmentConfiguration, previous *benchmarkReport) ([]benchmarkOptions, error) {
	throughput := common.Find(cases, func(options benchmarkOptions) bool { return options.Type != "memory" })
	iperf := throughput.iperf
	var iperfArchitecture string
	if target.Type != "local" {
		iperf = ""
	}
	var err error
	if throughput.Implementation != "" && iperf == "" && target.Type == "local" {
		iperf, err = exec.LookPath("iperf3")
		if err != nil && !errors.Is(err, exec.ErrNotFound) {
			return nil, E.Cause(err, "find iperf3")
		}
	}
	if throughput.Implementation != "" && iperf == "" {
		version := "latest"
		if previous != nil {
			fields := strings.Fields(previous.Iperf.Version)
			if len(fields) >= 2 && fields[0] == "iperf" {
				version = fields[1]
			}
		}
		binary, prepareErr := p.binary(ctx, "iperf3", version, "", target)
		iperfArchitecture = target.Arch
		if errors.Is(prepareErr, errUnsupportedRelease) && target.OS == "windows" && target.Arch == "arm64" {
			helperTarget := target
			helperTarget.Arch = "amd64"
			binary, prepareErr = p.binary(ctx, "iperf3", version, "", helperTarget)
			iperfArchitecture = helperTarget.Arch
			if prepareErr == nil {
				log.Info("Environment ", cases[0].environmentName, ": use amd64 iperf3 under Windows ARM64 emulation; TUN implementations remain arm64")
			}
		}
		if prepareErr != nil {
			if errors.Is(prepareErr, errUnsupportedRelease) {
				log.Warn("Environment ", cases[0].environmentName, ": skip throughput cases: ", prepareErr)
				cases = common.Filter(cases, func(options benchmarkOptions) bool { return options.Type == "memory" })
			} else {
				return nil, prepareErr
			}
		}
		iperf = binary.path
	}
	var supported []benchmarkOptions
	for _, options := range cases {
		options.iperf, options.iperfArchitecture = "", ""
		if options.Type != "memory" {
			options.iperf, options.iperfArchitecture = iperf, iperfArchitecture
		}
		binary, prepareErr := p.binary(ctx, options.software, options.version, options.sourcePackage, target)
		if prepareErr != nil {
			if errors.Is(prepareErr, errUnsupportedRelease) {
				log.Warn("Skipped ", options, ": ", prepareErr)
				continue
			}
			return nil, E.Cause(prepareErr, "prepare ", options.Implementation, " for ", target.OS, "/", target.Arch)
		}
		options.executable, options.version = binary.path, binary.version
		supported = append(supported, options)
	}
	if !slices.ContainsFunc(supported, benchmarkOptions.needsRelay) {
		return supported, nil
	}
	relayOptions := common.Find(supported, func(options benchmarkOptions) bool {
		return options.software == "sing-box" && options.sourcePackage != ""
	})
	if relayOptions.executable == "" {
		relayOptions = common.Find(supported, func(options benchmarkOptions) bool { return options.software == "sing-box" })
	}
	relayExecutable := relayOptions.executable
	if relayExecutable == "" {
		relayBinary, relayErr := p.binary(ctx, "sing-box", "latest", "", target)
		if relayErr != nil {
			return nil, E.Cause(relayErr, "prepare sing-box relay")
		}
		relayExecutable = relayBinary.path
	}
	for i := range supported {
		if supported[i].needsRelay() {
			supported[i].relayExecutable = relayExecutable
		}
	}
	return supported, nil
}

func (p *artifactPreparer) binary(ctx context.Context, software, version, sourcePackage string, target environmentConfiguration) (preparedBinary, error) {
	latest := sourcePackage == "" && version == "latest"
	if latest && p.versions[software] != "" {
		version = p.versions[software]
	}
	source := version
	if sourcePackage != "" {
		source = sourcePackage
	}
	key := [4]string{software, source, target.OS, target.Arch}
	binary, loaded := p.binaries[key]
	if loaded {
		return binary, nil
	}
	unsupported := p.unsupported[key]
	if unsupported != nil {
		return binary, unsupported
	}
	var err error
	if sourcePackage != "" {
		directory, createErr := os.MkdirTemp(p.directory, "package-*")
		if createErr != nil {
			return binary, createErr
		}
		binary.path = filepath.Join(directory, software+target.executableSuffix())
		packageName := "./cmd/sing-box"
		if software == "mihomo" {
			packageName = "."
		}
		err = buildExecutable(ctx, sourcePackage, packageName, binary.path, target, "with_gvisor")
	} else {
		log.Info("Prepare ", software, " ", version, " for ", target.OS, "/", target.Arch)
		p.client.operatingSystem, p.client.architecture = target.OS, target.Arch
		binary.path, binary.version, err = p.client.prepare(ctx, software, version, p.directory)
	}
	if err != nil {
		if errors.Is(err, errUnsupportedRelease) {
			p.unsupported[key] = err
		}
		return binary, err
	}
	if latest {
		p.versions[software] = binary.version
	}
	p.binaries[key] = binary
	if binary.version != "" {
		key[1] = binary.version
		p.binaries[key] = binary
	}
	return binary, nil
}

func (p *artifactPreparer) runner(ctx context.Context, target environmentConfiguration) (string, error) {
	key := [2]string{target.OS, target.Arch}
	executable, loaded := p.runners[key]
	if loaded {
		return executable, nil
	}
	directory, err := benchmarkSourceDirectory()
	if err != nil {
		return "", err
	}
	executable = filepath.Join(p.directory, "tun-bench-"+target.OS+"-"+target.Arch+target.executableSuffix())
	err = buildExecutable(ctx, directory, ".", executable, target, "")
	if err != nil {
		return "", err
	}
	p.runners[key] = executable
	return executable, nil
}

func benchmarkSourceDirectory() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	_, source, _, _ := runtime.Caller(0)
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{current, filepath.Dir(source), filepath.Dir(executable)} {
		if !filepath.IsAbs(candidate) {
			continue
		}
		for {
			content, readErr := os.ReadFile(filepath.Join(candidate, "go.mod"))
			if readErr == nil && strings.HasPrefix(string(content), "module github.com/sagernet/tun-bench\n") {
				return candidate, nil
			}
			parent := filepath.Dir(candidate)
			if parent == candidate {
				break
			}
			candidate = parent
		}
	}
	return "", E.New("cannot locate tun-bench source; run remote benchmarks from a tun-bench checkout")
}

func buildExecutable(ctx context.Context, directory, packageName, destination string, target environmentConfiguration, tags string) error {
	log.Info("Build ", packageName, " for ", target.OS, "/", target.Arch)
	args := []string{"build", "-trimpath", "-ldflags=-checklinkname=0", "-o", destination}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, packageName)
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GOOS="+target.OS, "GOARCH="+target.Arch, "CGO_ENABLED=0")
	var output processOutput
	captureCommandOutput(command, nil, os.Stderr, &output)
	err := command.Run()
	return commandError(command, err, &output)
}

func newArtifactPreparer(cache, directory string) (*artifactPreparer, error) {
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	apiOptions := []github.ClientOptionsFunc{github.WithHTTPClient(httpClient)}
	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		apiOptions = append(apiOptions, github.WithAuthToken(token))
	}
	api, err := github.NewClient(apiOptions...)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	return &artifactPreparer{
		client: releaseClient{client: httpClient, api: api, cache: cache}, directory: directory,
		binaries: make(map[[4]string]preparedBinary), unsupported: make(map[[4]string]error),
		runners: make(map[[2]string]string), versions: make(map[string]string),
	}, nil
}

func cacheDirectory() (string, error) {
	currentUser, err := user.Current()
	if err != nil {
		return "", err
	}
	var failures []error
	for _, temporary := range []bool{true, false} {
		var directory string
		if temporary {
			directory = filepath.Join(os.TempDir(), "tun-bench-"+currentUser.Uid)
		} else {
			directory, err = os.UserCacheDir()
			if err != nil {
				failures = append(failures, err)
				continue
			}
			directory = filepath.Join(directory, "tun-bench")
		}
		err = os.MkdirAll(directory, 0o700)
		if err == nil {
			var info os.FileInfo
			info, err = os.Lstat(directory)
			if err == nil {
				err = validateCacheDirectory(directory, info)
			}
		}
		if err == nil {
			var probe *os.File
			probe, err = os.CreateTemp(directory, ".probe-*")
			if err == nil {
				err = E.Errors(probe.Close(), os.Remove(probe.Name()))
			}
		}
		if err == nil {
			return directory, nil
		}
		failures = append(failures, E.Cause(err, "use cache ", directory))
	}
	return "", E.Errors(failures...)
}

package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"

	"github.com/google/go-github/v91/github"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/mod/semver"
)

var errUnsupportedRelease = E.New("unsupported release")

var releaseRepositories = map[string]string{
	"sing-box":            "SagerNet/sing-box",
	"hev-socks5-tunnel":   "heiher/hev-socks5-tunnel",
	"xjasonlyu-tun2socks": "xjasonlyu/tun2socks",
	"go-tun2socks":        "eycorsican/go-tun2socks",
	"leaf":                "eycorsican/leaf",
	"mihomo":              "MetaCubeX/mihomo",
	"v2ray":               "v2fly/v2ray-core",
	"xray":                "XTLS/Xray-core",
	"iperf3":              "userdocs/iperf3-static",
}

type releaseAsset struct {
	*github.ReleaseAsset
	owner      string
	repository string
}

type releaseClient struct {
	api             *github.Client
	client          *http.Client
	cache           string
	operatingSystem string
	architecture    string
}

func (c *releaseClient) prepare(ctx context.Context, software, version, workingDirectory string) (string, string, error) {
	actualVersion, asset, err := c.resolve(ctx, software, version)
	if err != nil {
		return "", "", err
	}
	archive, err := c.download(ctx, asset)
	if err != nil {
		return "", "", err
	}
	directory, err := os.MkdirTemp(workingDirectory, "bin-*")
	if err != nil {
		return "", "", err
	}
	executable, err := extractRelease(archive, software, directory, c.operatingSystem)
	if err == nil && c.operatingSystem == "windows" && (software == "xjasonlyu-tun2socks" || software == "leaf" || software == "xray") {
		err = c.prepareWintun(ctx, directory)
	}
	if err == nil && c.operatingSystem == "windows" && software == "hev-socks5-tunnel" {
		err = c.prepareMSYSSignal(ctx, directory)
	}
	if err != nil {
		err = E.Append(err, os.RemoveAll(directory), func(removeErr error) error { return E.Cause(removeErr, "remove incomplete executable") })
		return "", "", E.Cause(err, "extract ", asset.GetName())
	}
	return executable, actualVersion, nil
}

func (c *releaseClient) resolve(ctx context.Context, software, version string) (string, releaseAsset, error) {
	repository, loaded := releaseRepositories[software]
	if !loaded {
		return "", releaseAsset{}, E.New("no release provider for ", software)
	}
	owner, repositoryName, _ := strings.Cut(repository, "/")
	var release *github.RepositoryRelease
	var err error
	directory := filepath.Join(c.cache, "releases", owner, repositoryName)
	cached := false
	if version != "latest" {
		content, readErr := os.ReadFile(filepath.Join(directory, version+".json"))
		if readErr == nil {
			decodeErr := json.Unmarshal(content, &release)
			cached = decodeErr == nil && release != nil && strings.TrimPrefix(release.GetTagName(), "v") == version
		}
	}
	if !cached {
		if version == "latest" {
			release, _, err = c.api.Repositories.GetLatestRelease(ctx, owner, repositoryName)
		} else {
			tag := version
			if software != "hev-socks5-tunnel" && software != "iperf3" {
				tag = "v" + version
			}
			release, _, err = c.api.Repositories.GetReleaseByTag(ctx, owner, repositoryName, tag)
		}
		if err != nil {
			return "", releaseAsset{}, err
		}
	}
	actualVersion := strings.TrimPrefix(release.GetTagName(), "v")
	if !semver.IsValid("v"+actualVersion) || version != "latest" && version != actualVersion {
		return "", releaseAsset{}, E.New("unexpected release tag: ", release.GetTagName())
	}
	name, err := releaseAssetName(software, actualVersion, c.operatingSystem, c.architecture)
	if err != nil {
		return "", releaseAsset{}, err
	}
	asset := common.Find(release.Assets, func(candidate *github.ReleaseAsset) bool { return candidate.GetName() == name })
	if software == "iperf3" && c.operatingSystem == "darwin" {
		var minimum int
		for _, candidate := range release.Assets {
			value, matched := strings.CutPrefix(candidate.GetName(), name)
			if !matched {
				continue
			}
			target, parseErr := strconv.Atoi(value)
			if parseErr != nil || target <= 0 {
				continue
			}
			if asset == nil || target < minimum {
				asset, minimum = candidate, target
			}
		}
	}
	if asset == nil {
		return "", releaseAsset{}, E.Extend(errUnsupportedRelease, "release ", release.GetTagName(), " has no native ", software, " binary for ", c.operatingSystem, "/", c.architecture)
	}
	if asset.GetID() <= 0 || asset.GetSize() <= 0 {
		return "", releaseAsset{}, E.New("invalid release asset: ", asset.GetName())
	}
	if !cached {
		content, encodeErr := json.Marshal(release)
		if encodeErr != nil {
			return "", releaseAsset{}, E.Cause(encodeErr, "encode release metadata")
		}
		err = os.MkdirAll(directory, 0o700)
		if err != nil {
			return "", releaseAsset{}, E.Cause(err, "create release cache")
		}
		file, createErr := os.CreateTemp(directory, ".release-*")
		if createErr != nil {
			return "", releaseAsset{}, createErr
		}
		defer os.Remove(file.Name())
		_, err = file.Write(content)
		err = E.Append(err, file.Close(), func(closeErr error) error { return E.Cause(closeErr, "close release metadata") })
		if err != nil {
			return "", releaseAsset{}, err
		}
		err = os.Rename(file.Name(), filepath.Join(directory, actualVersion+".json"))
		if err != nil {
			return "", releaseAsset{}, E.Cause(err, "cache release metadata")
		}
	}
	return actualVersion, releaseAsset{ReleaseAsset: asset, owner: owner, repository: repositoryName}, nil
}

func releaseAssetName(software, version, operatingSystem, architecture string) (string, error) {
	switch software {
	case "v2ray", "xray":
		switch architecture {
		case "amd64":
			architecture = "64"
		case "386":
			architecture = "32"
		case "arm":
			architecture = "arm32-v5"
		case "arm64":
			architecture = "arm64-v8a"
		case "mips":
			architecture = "mips32"
		case "mipsle":
			architecture = "mips32le"
		}
		if operatingSystem == "darwin" {
			operatingSystem = "macos"
		}
		name := software
		if software == "xray" {
			name = "Xray"
		}
		return name + "-" + operatingSystem + "-" + architecture + ".zip", nil
	case "go-tun2socks":
		switch operatingSystem {
		case "linux":
			if architecture == "arm" {
				architecture = "arm-5"
			}
			return "tun2socks-linux-" + architecture, nil
		case "darwin":
			return "tun2socks-darwin-10.6-" + architecture, nil
		default:
			return "", E.Extend(errUnsupportedRelease, "no ", software, " benchmark support for ", operatingSystem)
		}
	case "xjasonlyu-tun2socks":
		switch architecture {
		case "arm":
			architecture = "armv5"
		case "mips", "mipsle":
			architecture += "-softfloat"
		}
		return "tun2socks-" + operatingSystem + "-" + architecture + ".zip", nil
	case "leaf":
		switch architecture {
		case "amd64":
			architecture = "x86_64"
		case "arm64":
			architecture = "aarch64"
		}
		var target string
		extension := ".gz"
		switch operatingSystem {
		case "darwin":
			target = "apple-darwin"
		case "windows":
			target = "pc-windows-gnu"
			extension = ".zip"
		case "linux":
			target = "unknown-linux-musl"
		default:
			return "", E.Extend(errUnsupportedRelease, "no native ", software, " release for ", operatingSystem, "/", architecture)
		}
		return software + "-" + architecture + "-" + target + extension, nil
	case "mihomo":
		switch architecture {
		case "amd64":
			architecture = "amd64-v1"
		case "arm":
			architecture = "armv5"
		case "mips", "mipsle":
			architecture += "-softfloat"
		case "loong64":
			architecture = "loong64-abi2"
		}
		extension := ".gz"
		if operatingSystem == "windows" {
			extension = ".zip"
		}
		return "mihomo-" + operatingSystem + "-" + architecture + "-v" + version + extension, nil
	case "sing-box":
		switch architecture {
		case "arm":
			architecture = "armv5"
		case "mips", "mips64":
			architecture += "-softfloat"
		}
		extension := ".tar.gz"
		if operatingSystem == "windows" {
			extension = ".zip"
		}
		return "sing-box-" + version + "-" + operatingSystem + "-" + architecture + extension, nil
	case "hev-socks5-tunnel":
		if operatingSystem == "windows" {
			if architecture != "amd64" {
				return "", E.Extend(errUnsupportedRelease, "no native ", software, " release for windows/", architecture)
			}
			return "hev-socks5-tunnel-win64.zip", nil
		}
		switch architecture {
		case "amd64":
			architecture = "x86_64"
		case "386":
			architecture = "i686"
		case "arm":
			architecture = "arm32"
		case "mips":
			architecture = "mips32sf"
		case "mipsle":
			architecture = "mips32elsf"
		case "mips64le":
			architecture = "mips64el"
		case "ppc64":
			architecture = "powerpc64"
		case "ppc64le":
			architecture = "powerpc64le"
		}
		return "hev-socks5-tunnel-" + operatingSystem + "-" + architecture, nil
	case "iperf3":
		switch operatingSystem {
		case "windows":
			if architecture != "amd64" {
				return "", E.Extend(errUnsupportedRelease, "no native iperf3 release for windows/", architecture, "; configure iperf3")
			}
			return "iperf3-amd64-win.zip", nil
		case "darwin":
			return "iperf3-" + architecture + "-osx-", nil
		case "linux":
			switch architecture {
			case "386":
				architecture = "i386"
			case "arm":
				architecture = "arm32v5"
			case "arm64":
				architecture = "arm64v8"
			case "loong64":
				architecture = "loongarch64"
			}
			return "iperf3-" + architecture, nil
		default:
			return "", E.Extend(errUnsupportedRelease, "no native iperf3 release for ", operatingSystem, "/", architecture, "; configure iperf3")
		}
	default:
		return "", E.New("no release provider for ", software)
	}
}

func (c *releaseClient) download(ctx context.Context, asset releaseAsset) (string, error) {
	directory := filepath.Join(c.cache, "downloads", strconv.FormatInt(asset.GetID(), 10))
	destination := filepath.Join(directory, asset.GetName())
	err := asset.verify(destination)
	if err == nil {
		return destination, nil
	}
	err = os.MkdirAll(directory, 0o700)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	var content io.ReadCloser
	if asset.GetBrowserDownloadURL() != "" {
		var request *http.Request
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, asset.GetBrowserDownloadURL(), nil)
		if err != nil {
			return "", err
		}
		var response *http.Response
		response, err = c.client.Do(request)
		if err == nil {
			if response.StatusCode != http.StatusOK {
				response.Body.Close()
				return "", E.New("download ", asset.GetName(), ": ", response.Status)
			}
			content = response.Body
		}
	} else {
		content, _, err = c.api.Repositories.DownloadReleaseAsset(ctx, asset.owner, asset.repository, asset.GetID(), c.client)
	}
	if err != nil {
		return "", err
	}
	defer content.Close()
	_, err = bufio.Copy(file, io.LimitReader(content, int64(asset.GetSize())+1))
	err = E.Append(err, file.Close(), func(closeErr error) error { return E.Cause(closeErr, "close download") })
	if err != nil {
		return "", err
	}
	err = asset.verify(file.Name())
	if err != nil {
		return "", err
	}
	err = os.Rename(file.Name(), destination)
	if err != nil {
		return "", E.Cause(err, "cache download")
	}
	return destination, nil
}

func (c *releaseClient) prepareWintun(ctx context.Context, directory string) error {
	destination := filepath.Join(directory, "wintun.dll")
	_, err := os.Stat(destination)
	if err == nil {
		return nil
	}
	archivePath, err := c.download(ctx, releaseAsset{ReleaseAsset: &github.ReleaseAsset{
		Name: new("wintun-0.14.1.zip"), Size: new(750540),
		BrowserDownloadURL: new("https://www.wintun.net/builds/wintun-0.14.1.zip"),
		Digest:             new("sha256:07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51"),
	}})
	if err != nil {
		return E.Cause(err, "download Wintun")
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	architecture := c.architecture
	if architecture == "386" {
		architecture = "x86"
	}
	for _, entry := range archive.File {
		name := ""
		switch entry.Name {
		case "wintun/bin/" + architecture + "/wintun.dll":
			name = destination
		case "wintun/LICENSE.txt":
			name = filepath.Join(directory, "wintun-LICENSE.txt")
		default:
			continue
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return openErr
		}
		err = writeReleaseFile(name, reader)
		err = E.Append(err, reader.Close(), func(closeErr error) error { return E.Cause(closeErr, "close Wintun archive entry") })
		if err != nil {
			return err
		}
	}
	_, err = os.Stat(destination)
	return err
}

func (a releaseAsset) verify(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(a.GetSize()) {
		return E.New("release asset size mismatch")
	}
	if a.GetDigest() == "" {
		return nil
	}
	digest, supported := strings.CutPrefix(a.GetDigest(), "sha256:")
	if !supported {
		return E.New("unsupported release digest: ", a.GetDigest())
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return E.New("release asset SHA-256 mismatch")
	}
	return nil
}

func extractRelease(archivePath, software, directory, operatingSystem string) (string, error) {
	executable := software
	if software == "xjasonlyu-tun2socks" || software == "go-tun2socks" {
		executable = "tun2socks"
	}
	if operatingSystem == "windows" {
		executable += ".exe"
	}
	destination := filepath.Join(directory, executable)
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		archive, err := zip.OpenReader(archivePath)
		if err != nil {
			return "", err
		}
		defer archive.Close()
		for _, entry := range archive.File {
			name := path.Base(entry.Name)
			if software == "xjasonlyu-tun2socks" || software == "mihomo" || software == "leaf" {
				archiveExecutable := strings.TrimSuffix(filepath.Base(archivePath), ".zip")
				if software == "mihomo" {
					versionIndex := strings.LastIndex(archiveExecutable, "-v")
					if versionIndex != -1 {
						archiveExecutable = archiveExecutable[:versionIndex]
					}
				}
				if operatingSystem == "windows" && !strings.HasSuffix(archiveExecutable, ".exe") {
					archiveExecutable += ".exe"
				}
				if name == archiveExecutable {
					name = executable
				}
			}
			if name != executable && !strings.HasSuffix(strings.ToLower(name), ".dll") {
				continue
			}
			if !entry.Mode().IsRegular() {
				return "", E.New("non-regular release file: ", entry.Name)
			}
			reader, openErr := entry.Open()
			if openErr != nil {
				return "", openErr
			}
			err = writeReleaseFile(filepath.Join(directory, name), reader)
			err = E.Append(err, reader.Close(), func(closeErr error) error { return E.Cause(closeErr, "close archive entry") })
			if err != nil {
				return "", err
			}
		}
	case strings.HasSuffix(archivePath, ".tar.gz"):
		file, err := os.Open(archivePath)
		if err != nil {
			return "", err
		}
		defer file.Close()
		reader, err := gzip.NewReader(file)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		archive := tar.NewReader(reader)
		for {
			entry, nextErr := archive.Next()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				return "", nextErr
			}
			if path.Base(entry.Name) != executable {
				continue
			}
			if !entry.FileInfo().Mode().IsRegular() {
				return "", E.New("non-regular release file: ", entry.Name)
			}
			err = writeReleaseFile(destination, archive)
			if err != nil {
				return "", err
			}
		}
	case strings.HasSuffix(archivePath, ".gz"):
		file, err := os.Open(archivePath)
		if err != nil {
			return "", err
		}
		defer file.Close()
		reader, err := gzip.NewReader(file)
		if err != nil {
			return "", err
		}
		defer reader.Close()
		err = writeReleaseFile(destination, reader)
		if err != nil {
			return "", err
		}
	default:
		file, err := os.Open(archivePath)
		if err != nil {
			return "", err
		}
		defer file.Close()
		err = writeReleaseFile(destination, file)
		if err != nil {
			return "", err
		}
	}
	info, err := os.Stat(destination)
	if err != nil {
		return "", E.Cause(err, "find ", executable, " in release")
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", E.New("invalid release executable: ", executable)
	}
	return destination, nil
}

func writeReleaseFile(destination string, content io.Reader) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, content)
	return E.Append(err, file.Close(), func(closeErr error) error { return E.Cause(closeErr, "close ", destination) })
}

func (c *releaseClient) prepareMSYSSignal(ctx context.Context, directory string) error {
	_, err := os.Stat(filepath.Join(directory, "msys-2.0.dll"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	archivePath, err := c.download(ctx, releaseAsset{ReleaseAsset: &github.ReleaseAsset{
		Name: new("msys2-runtime-3.6.9-2-x86_64.pkg.tar.zst"), Size: new(2008283),
		BrowserDownloadURL: new("https://repo.msys2.org/msys/x86_64/msys2-runtime-3.6.9-2-x86_64.pkg.tar.zst"),
		Digest:             new("sha256:20f39ad6d0fd2aae93ca84c2e9efbe567d3d7f5d465dc0810b700d7cbac0c3a4"),
	}})
	if err != nil {
		return E.Cause(err, "download MSYS signal helper")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	for {
		entry, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			return E.New("MSYS package has no kill.exe")
		}
		if nextErr != nil {
			return nextErr
		}
		if entry.Name != "usr/bin/kill.exe" {
			continue
		}
		if !entry.FileInfo().Mode().IsRegular() {
			return E.New("non-regular MSYS signal helper")
		}
		return writeReleaseFile(filepath.Join(directory, "msys-kill.exe"), archive)
	}
}

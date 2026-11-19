// Command release packages codex-reorder libraries in the CPA plugin store
// format and verifies the result with CPA's own installer.
//
//	go run ./tools/release package -version 0.6.5 -lib linux/amd64=dist/lib/linux_amd64/codex-reorder.so ...
//	go run ./tools/release verify  -version 0.6.5 -require linux/amd64,linux/arm64
//
// The store expects, per GitHub Release tagged v<version>:
//
//	codex-reorder_<version>_<goos>_<goarch>.zip   (codex-reorder.so at the zip root)
//	checksums.txt                                 (sha256sum format)
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

const (
	pluginID   = "codex-reorder"
	repoOwner  = "Ca11back"
	repoName   = "cpa-codex-reorder"
	repository = "https://github.com/" + repoOwner + "/" + repoName
)

// Symbols CPA resolves after dlopen (see abi.go).
var abiSymbols = []string{"cliproxy_plugin_init", "cliproxyPluginCall", "cliproxyPluginFree", "cliproxyPluginShutdown"}

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "package":
		err = runPackage(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release package|verify [flags]  (-h for flags)")
	os.Exit(2)
}

type libFlag map[string]string // "goos/goarch" -> library path

func (l libFlag) String() string { return fmt.Sprint(map[string]string(l)) }

func (l libFlag) Set(value string) error {
	platform, path, ok := strings.Cut(value, "=")
	if !ok || strings.Count(platform, "/") != 1 || path == "" {
		return fmt.Errorf("want goos/goarch=path, got %q", value)
	}
	l[platform] = path
	return nil
}

func runPackage(args []string) error {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	version := fs.String("version", "", "plugin version without the v prefix")
	out := fs.String("out", "dist/release", "output directory")
	libs := libFlag{}
	fs.Var(libs, "lib", "goos/goarch=path to a built library (repeatable)")
	_ = fs.Parse(args)
	if err := checkVersion(*version); err != nil {
		return err
	}
	if len(libs) == 0 {
		return errors.New("no -lib given")
	}
	if err := os.RemoveAll(*out); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	platforms := make([]string, 0, len(libs))
	for platform := range libs {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	var checksums strings.Builder
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		if goos != "linux" {
			return fmt.Errorf("%s: only linux libraries are supported", platform)
		}
		library, err := os.ReadFile(libs[platform])
		if err != nil {
			return err
		}
		archive, err := zipLibrary(pluginID+".so", library)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s_%s_%s_%s.zip", pluginID, *version, goos, goarch)
		if err := os.WriteFile(filepath.Join(*out, name), archive, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(archive)
		fmt.Fprintf(&checksums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(*out, name), len(archive))
	}
	if err := os.WriteFile(filepath.Join(*out, "checksums.txt"), []byte(checksums.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", filepath.Join(*out, "checksums.txt"))
	return nil
}

// zipLibrary builds a deterministic zip holding one executable file at the root.
func zipLibrary(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	header.SetMode(0o755)
	f, err := w.CreateHeader(header)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	version := fs.String("version", "", "plugin version without the v prefix")
	dir := fs.String("dir", "dist/release", "directory holding the release assets")
	require := fs.String("require", "linux/amd64,linux/arm64", "comma-separated platforms that must be present")
	maxGlibc := fs.String("max-glibc", "2.17", "highest GLIBC symbol version a library may need")
	_ = fs.Parse(args)
	if err := checkVersion(*version); err != nil {
		return err
	}
	if err := checkSourceVersion(*version); err != nil {
		return err
	}
	assets, err := readAssets(*dir)
	if err != nil {
		return err
	}
	if _, ok := assets["checksums.txt"]; !ok {
		return errors.New("checksums.txt missing")
	}
	if err := checkChecksumsCoverZips(assets); err != nil {
		return err
	}

	tag := "v" + *version
	client := pluginstore.NewClient(&fakeGitHub{tag: tag, assets: assets}, "")
	plugin := pluginstore.Plugin{
		ID:          pluginID,
		Name:        "Codex Reorder",
		Description: "release verification",
		Author:      repoOwner,
		Repository:  repository,
	}
	for _, platform := range strings.Split(*require, ",") {
		platform = strings.TrimSpace(platform)
		goos, goarch, ok := strings.Cut(platform, "/")
		if !ok {
			return fmt.Errorf("bad platform %q", platform)
		}
		pluginsDir, err := os.MkdirTemp("", "codex-reorder-verify-")
		if err != nil {
			return err
		}
		result, err := client.InstallVersion(context.Background(), plugin, tag, *version, pluginstore.InstallOptions{
			PluginsDir: pluginsDir, GOOS: goos, GOARCH: goarch,
		})
		if err != nil {
			os.RemoveAll(pluginsDir)
			return fmt.Errorf("%s: CPA installer rejected the release: %w", platform, err)
		}
		errLib := checkLibrary(result.Path, goarch, *maxGlibc)
		os.RemoveAll(pluginsDir)
		if errLib != nil {
			return fmt.Errorf("%s: %w", platform, errLib)
		}
		fmt.Printf("ok  %s  installs as %s\n", platform, filepath.Join(goos, goarch, filepath.Base(result.Path)))
	}
	return nil
}

func checkVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("version %q must look like 1.2.3 (no v prefix)", version)
	}
	return nil
}

// checkSourceVersion makes sure the tag matches the version the plugin reports.
func checkSourceVersion(version string) error {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		return fmt.Errorf("read plugin.go (run from the repository root): %w", err)
	}
	m := regexp.MustCompile(`pluginVersion\s*=\s*"([^"]+)"`).FindSubmatch(src)
	if m == nil {
		return errors.New("pluginVersion not found in plugin.go")
	}
	if string(m[1]) != version {
		return fmt.Errorf("plugin.go pluginVersion is %s, release version is %s", m[1], version)
	}
	return nil
}

func readAssets(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	assets := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		assets[entry.Name()] = data
	}
	return assets, nil
}

func checkChecksumsCoverZips(assets map[string][]byte) error {
	listed := map[string]bool{}
	for _, line := range strings.Split(string(assets["checksums.txt"]), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			listed[fields[1]] = true
		}
	}
	for name := range assets {
		if strings.HasSuffix(name, ".zip") && !listed[name] {
			return fmt.Errorf("%s is not listed in checksums.txt", name)
		}
	}
	return nil
}

// checkLibrary inspects the library exactly as CPA installed it.
func checkLibrary(path, goarch, maxGlibc string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch]
	if f.Machine != want {
		return fmt.Errorf("ELF machine %v, want %v", f.Machine, want)
	}
	if f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %v, want shared object", f.Type)
	}
	symbols, err := f.DynamicSymbols()
	if err != nil {
		return err
	}
	exported := map[string]bool{}
	for _, s := range symbols {
		if s.Section != elf.SHN_UNDEF {
			exported[s.Name] = true
		}
	}
	for _, name := range abiSymbols {
		if !exported[name] {
			return fmt.Errorf("missing exported symbol %s", name)
		}
	}
	imported, err := f.ImportedSymbols()
	if err != nil {
		return err
	}
	highest, highestSym := "", ""
	for _, s := range imported {
		v, ok := strings.CutPrefix(s.Version, "GLIBC_")
		if ok && compareVersions(v, highest) > 0 {
			highest, highestSym = v, s.Name
		}
	}
	if highest != "" && compareVersions(highest, maxGlibc) > 0 {
		return fmt.Errorf("needs GLIBC_%s (symbol %s), above the GLIBC_%s limit", highest, highestSym, maxGlibc)
	}
	return nil
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// fakeGitHub answers the two kinds of requests CPA's installer makes: the
// release-by-tag API call and the asset downloads, served from local files.
type fakeGitHub struct {
	tag    string
	assets map[string][]byte
}

func (g *fakeGitHub) Do(req *http.Request) (*http.Response, error) {
	releaseAPI := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", repoOwner, repoName, g.tag)
	downloadPrefix := fmt.Sprintf("/%s/%s/releases/download/%s/", repoOwner, repoName, g.tag)
	switch {
	case req.URL.Host == "api.github.com" && req.URL.Path == releaseAPI:
		type asset struct {
			URL                string `json:"url"`
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}
		release := struct {
			TagName string  `json:"tag_name"`
			Assets  []asset `json:"assets"`
		}{TagName: g.tag}
		for name := range g.assets {
			release.Assets = append(release.Assets, asset{
				URL:                "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/assets/" + name,
				Name:               name,
				BrowserDownloadURL: "https://github.com" + downloadPrefix + name,
			})
		}
		body, _ := json.Marshal(release)
		return response(req, http.StatusOK, body), nil
	case req.URL.Host == "github.com" && strings.HasPrefix(req.URL.Path, downloadPrefix):
		if data, ok := g.assets[strings.TrimPrefix(req.URL.Path, downloadPrefix)]; ok {
			return response(req, http.StatusOK, data), nil
		}
	}
	return response(req, http.StatusNotFound, []byte(`{"message":"Not Found"}`)), nil
}

func response(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"Content-Type": {"application/octet-stream"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

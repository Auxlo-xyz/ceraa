package tool

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// browserProvisionTimeout bounds one repair. The first attempt also fetches the
// distribution's package index, which is the slow part; the libraries
// themselves are a few megabytes. The repair runs on its own context because
// the tool's deadline is set for launching a browser, not for repairing a host,
// and a download cut in half leaves the next attempt with a broken cache.
const browserProvisionTimeout = 5 * time.Minute

// browserDeb maps libraries Chromium needs to the packages that provide them.
//
// Both the time_t-transition package names (Ubuntu 24.04 and later, Debian 13)
// and their predecessors are listed because the distributions disagree; only
// the names a host's own package index knows about are fetched. The list is
// data, not policy: a library that is present on the host is never fetched, so
// nothing here shadows a working system library.
type browserDeb struct {
	names   []string
	sonames []string
}

var browserDebs = []browserDeb{
	{[]string{"libasound2t64", "libasound2"}, []string{"libasound.so.2"}},
	{[]string{"libatk1.0-0t64", "libatk1.0-0"}, []string{"libatk-1.0.so.0"}},
	{[]string{"libatk-bridge2.0-0t64", "libatk-bridge2.0-0"}, []string{"libatk-bridge-2.0.so.0"}},
	{[]string{"libatspi2.0-0t64", "libatspi2.0-0"}, []string{"libatspi.so.0"}},
	{[]string{"libcairo2"}, []string{"libcairo.so.2"}},
	{[]string{"libcups2t64", "libcups2"}, []string{"libcups.so.2"}},
	{[]string{"libdbus-1-3"}, []string{"libdbus-1.so.3"}},
	{[]string{"libdrm2"}, []string{"libdrm.so.2"}},
	{[]string{"libgbm1"}, []string{"libgbm.so.1"}},
	{[]string{"libglib2.0-0t64", "libglib2.0-0"}, []string{"libglib-2.0.so.0"}},
	{[]string{"libnspr4"}, []string{"libnspr4.so", "libplc4.so", "libplds4.so"}},
	{[]string{"libnss3"}, []string{"libnss3.so", "libnssutil3.so", "libsmime3.so", "libssl3.so"}},
	{[]string{"libpango-1.0-0"}, []string{"libpango-1.0.so.0"}},
	{[]string{"libx11-6"}, []string{"libX11.so.6"}},
	{[]string{"libxcb1"}, []string{"libxcb.so.1"}},
	{[]string{"libxcomposite1"}, []string{"libXcomposite.so.1"}},
	{[]string{"libxdamage1"}, []string{"libXdamage.so.1"}},
	{[]string{"libxext6"}, []string{"libXext.so.6"}},
	{[]string{"libxfixes3"}, []string{"libXfixes.so.3"}},
	{[]string{"libxkbcommon0"}, []string{"libxkbcommon.so.0"}},
	{[]string{"libxrandr2"}, []string{"libXrandr.so.2"}},

	// Dependencies of the libraries above. They are fetched only when the
	// loader reports them missing, which is how they appear after the first
	// pass unpacks the library that needs them.
	{[]string{"libxi6"}, []string{"libXi.so.6"}},
	{[]string{"libxtst6"}, []string{"libXtst.so.6"}},
	{[]string{"libxau6"}, []string{"libXau.so.6"}},
	{[]string{"libxdmcp6"}, []string{"libXdmcp.so.6"}},
	{[]string{"libxrender1"}, []string{"libXrender.so.1"}},
	{[]string{"libthai0"}, []string{"libthai.so.0"}},
	{[]string{"libdatrie1"}, []string{"libdatrie.so.1"}},
	{[]string{"libfribidi0"}, []string{"libfribidi.so.0"}},
	{[]string{"libharfbuzz0b"}, []string{"libharfbuzz.so.0"}},
	{[]string{"libfontconfig1"}, []string{"libfontconfig.so.1"}},
	{[]string{"libfreetype6"}, []string{"libfreetype.so.6"}},
	{[]string{"libpng16-16t64", "libpng16-16"}, []string{"libpng16.so.16"}},
	{[]string{"libgraphite2-3"}, []string{"libgraphite2.so.3"}},
	{[]string{"libbrotli1"}, []string{"libbrotlidec.so.1"}},
	{[]string{"libbz2-1.0"}, []string{"libbz2.so.1.0"}},
	{[]string{"libexpat1"}, []string{"libexpat.so.1"}},
	{[]string{"libpcre2-8-0"}, []string{"libpcre2-8.so.0"}},
	{[]string{"libxml2"}, []string{"libxml2.so.2"}},
	{[]string{"libpixman-1-0"}, []string{"libpixman-1.so.0"}},
	{[]string{"libwayland-server0"}, []string{"libwayland-server.so.0"}},
	{[]string{"libxshmfence1"}, []string{"libxshmfence.so.1"}},
	{[]string{"libffi8"}, []string{"libffi.so.8"}},
	{[]string{"libselinux1"}, []string{"libselinux.so.1"}},
	{[]string{"libsystemd0"}, []string{"libsystemd.so.0"}},
	{[]string{"libcap2"}, []string{"libcap.so.2"}},
	{[]string{"libmount1"}, []string{"libmount.so.1"}},
	{[]string{"libblkid1"}, []string{"libblkid.so.1"}},
	{[]string{"liblzma5"}, []string{"liblzma.so.5"}},
	{[]string{"libzstd1"}, []string{"libzstd.so.1"}},
}

var browserProvisionMu sync.Mutex

// provisionBrowserLibraries makes the browser launchable on a host that does
// not install Chromium's shared libraries system-wide, without root.
//
// The libraries are downloaded with the host's own package manager and
// unpacked with dpkg-deb, exactly the manual procedure the loader hint used to
// describe, into the managed directory the launch path already searches. It is
// attempted only after a launch failed with a loader error, so a working host
// never downloads anything.
//
// This is a repair of last resort, not a dependency installer: packages whose
// libraries the loader already resolves are never fetched, so it cannot shadow
// a working system library with one from the repository.
func provisionBrowserLibraries(binary string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("automatic library provisioning is only implemented on linux")
	}
	for _, tool := range []string{"apt-get", "apt-cache", "dpkg-deb"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not installed: %w", tool, err)
		}
	}

	browserProvisionMu.Lock()
	defer browserProvisionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), browserProvisionTimeout)
	defer cancel()

	tmp, err := os.MkdirTemp("", "ceraa-browser-deps-")
	if err != nil {
		return fmt.Errorf("create a working directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	apt := browserAptDirs{
		lists:   filepath.Join(tmp, "lists"),
		cache:   filepath.Join(tmp, "cache"),
		archive: filepath.Join(tmp, "archives"),
	}
	for _, dir := range []string{
		filepath.Join(apt.lists, "partial"),
		filepath.Join(apt.cache, "archives", "partial"),
		apt.archive,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	if err := runBrowserDepsCommand(ctx, tmp, "apt-get", apt.args("update")...); err != nil {
		return fmt.Errorf("read the package index: %w", err)
	}

	target := filepath.Join(browserDownloadDir(), "libs")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}

	// Up to five passes: the first unpacks the libraries the loader named,
	// and each next one unpacks what those libraries turned out to need. Each
	// pass is cheap because the package index is already local.
	for pass := 0; pass < 5; pass++ {
		missing, err := missingBrowserLibs(binary)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			return nil
		}
		names, unknown := browserDebNamesFor(missing)
		if len(names) == 0 {
			return fmt.Errorf("the browser needs %s, and no package in this list provides %s",
				strings.Join(unknown, ", "), plural(len(unknown), "it", "them"))
		}
		available, err := availableBrowserDebs(ctx, tmp, apt, names)
		if err != nil {
			return err
		}
		if len(available) == 0 {
			return fmt.Errorf("no package in this distribution provides %s",
				strings.Join(unknownNames(missing), ", "))
		}
		log.Printf("browser: provisioning %d missing shared libraries with %d packages", len(missing), len(available))
		if err := downloadBrowserDebs(ctx, tmp, apt, available); err != nil {
			return err
		}
		if err := extractBrowserDebs(ctx, tmp); err != nil {
			return err
		}
		copied, err := flattenBrowserLibs(tmp, target)
		if err != nil {
			return err
		}
		log.Printf("browser: unpacked %d library files into %s", copied, target)
	}

	missing, err := missingBrowserLibs(binary)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("still missing after provisioning: %s", strings.Join(unknownNames(missing), ", "))
	}
	return nil
}

// browserAptDirs keeps apt's state out of /var, which a non-root user cannot
// write. The package index is disposable, so it lives in the provisioning
// directory and is removed with it.
type browserAptDirs struct {
	lists   string
	cache   string
	archive string
}

func (a browserAptDirs) args(args ...string) []string {
	return append([]string{
		"-o", "Dir::State::lists=" + a.lists,
		"-o", "Dir::Cache=" + a.cache,
		"-o", "Dir::Cache::archives=" + a.archive,
		"-o", "Acquire::Languages=none",
		"-o", "APT::Get::List-Cleanup=0",
	}, args...)
}

// missingBrowserLibs runs the loader against the browser with the same
// environment the launch uses and returns the libraries it cannot resolve.
func missingBrowserLibs(binary string) (map[string]bool, error) {
	cmd := exec.Command("ldd", binary)
	cmd.Env = browserLaunchEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w: %s", binary, err, tailText(string(out), 300))
	}
	missing := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "not" && i+1 < len(fields) && fields[i+1] == "found" && i > 0 {
				missing[fields[0]] = true
			}
		}
	}
	return missing, nil
}

// browserDebNamesFor returns every package name that could provide one of the
// missing libraries, and the libraries nothing in the table maps.
func browserDebNamesFor(missing map[string]bool) (names, unknown []string) {
	seen := make(map[string]bool)
	covered := make(map[string]bool)
	for _, deb := range browserDebs {
		needed := false
		for _, soname := range deb.sonames {
			if missing[soname] {
				needed = true
				covered[soname] = true
			}
		}
		if !needed {
			continue
		}
		for _, name := range deb.names {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	for soname := range missing {
		if !covered[soname] {
			unknown = append(unknown, soname)
		}
	}
	sort.Strings(unknown)
	return names, unknown
}

// availableBrowserDebs keeps the names the host's package index actually
// knows. apt-cache policy is a local query, so asking about both the current
// and the previous spelling costs nothing.
func availableBrowserDebs(ctx context.Context, dir string, apt browserAptDirs, names []string) ([]string, error) {
	out, err := runBrowserDepsOutput(ctx, dir, "apt-cache", apt.args(append([]string{"policy"}, names...)...)...)
	if err != nil {
		return nil, fmt.Errorf("query the package index: %w", err)
	}
	candidates := make(map[string]string)
	current := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(trimmed, ":") {
			current = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if current != "" && strings.HasPrefix(trimmed, "Candidate:") {
			candidates[current] = strings.TrimSpace(strings.TrimPrefix(trimmed, "Candidate:"))
		}
	}
	var available []string
	for _, name := range names {
		switch candidates[name] {
		case "", "(none)":
			continue
		}
		available = append(available, name)
	}
	return available, nil
}

func downloadBrowserDebs(ctx context.Context, dir string, apt browserAptDirs, names []string) error {
	if err := runBrowserDepsCommand(ctx, dir, "apt-get", apt.args(append([]string{"download"}, names...)...)...); err != nil {
		return fmt.Errorf("download the packages: %w", err)
	}
	return nil
}

func extractBrowserDebs(ctx context.Context, dir string) error {
	debs, err := filepath.Glob(filepath.Join(dir, "*.deb"))
	if err != nil {
		return fmt.Errorf("list downloaded packages: %w", err)
	}
	if len(debs) == 0 {
		return fmt.Errorf("the package manager downloaded nothing")
	}
	for _, deb := range debs {
		if err := runBrowserDepsCommand(ctx, dir, "dpkg-deb", "-x", deb, dir); err != nil {
			return fmt.Errorf("unpack %s: %w", filepath.Base(deb), err)
		}
	}
	return nil
}

// flattenBrowserLibs copies every library the packages installed into one
// directory, because the launch path adds exactly that directory to the loader
// search path. Symlinks are recreated rather than followed: a package's
// `libfoo.so.1` is usually a link to `libfoo.so.1.2.3`, and copying the target
// over the link would leave a filename nothing depends on.
func flattenBrowserLibs(root, target string) (int, error) {
	var sources []string
	for _, pattern := range []string{
		filepath.Join(root, "usr", "lib", "*"),
		filepath.Join(root, "usr", "lib", "*", "*"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return 0, fmt.Errorf("scan %s: %w", pattern, err)
		}
		for _, match := range matches {
			info, err := os.Lstat(match)
			if err != nil || info.IsDir() {
				continue
			}
			sources = append(sources, match)
		}
	}
	sort.Strings(sources)
	copied := 0
	for _, src := range sources {
		if err := copyBrowserLib(src, filepath.Join(target, filepath.Base(src))); err != nil {
			return copied, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
		}
		copied++
	}
	return copied, nil
}

func copyBrowserLib(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(link, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func runBrowserDepsCommand(ctx context.Context, dir, name string, args ...string) error {
	out, err := runBrowserDepsOutput(ctx, dir, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tailText(out, 400))
	}
	return nil
}

func runBrowserDepsOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = browserDepsEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// browserDepsEnv is this process's environment without LD_LIBRARY_PATH. The
// package tools must use the system libraries, not a half-filled directory this
// repair is still populating.
func browserDepsEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LD_LIBRARY_PATH=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "DEBIAN_FRONTEND=noninteractive")
}

func unknownNames(missing map[string]bool) []string {
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func tailText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return "..." + text[len(text)-limit:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

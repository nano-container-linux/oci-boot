package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/ulikunitz/xz"

	progressbar "github.com/schollz/progressbar/v3"

	"github.com/vishvananda/netlink"
)

type BlobStream struct {
	io.Reader
	Close func() error
}

const ANSI_DARKBLUE_BG = "\033[44m"
const ANSI_WHITE_FG = "\033[97m"
const ANSI_RESET = "\033[0m"

// MODULES_SYMLINK devient dynamique
const OCI_IMAGE_MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json"

var execName string

const PARAM_OCI_HTTP = "oci_http"
const PARAM_MODLOOP = "modloop"
const PARAM_ROOTFS = "rootfs"

const SCHEME_HTTP = "http://"
const SCHEME_HTTPS = "https://"
const SCHEME_OCI = "oci://"

const MOUNTPOINT_FS_PROC = "/proc"
const MOUNTPOINT_FS_SYS = "/sys"
const MOUNTPOINT_FS_DEV = "/dev"

func main() {
	execName = os.Args[0]
	os.Setenv("TERM", "linux")
	log.Print(ANSI_DARKBLUE_BG + ANSI_WHITE_FG + fmt.Sprintf("execName: %s", execName) + ANSI_RESET)

	if strings.HasSuffix(execName, "/init") {
		bootstrapInit()
	} else {
		bootstrapInitOCI()
	}
}

func readCmdline() map[string]string {
	cmdlineBytes, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		logFatalWithPrefix("Failed to read /proc/cmdline: %v", err)
	}
	return parseCmdline(string(cmdlineBytes))
}
func logFatalWithPrefix(format string, v ...interface{}) {
	log.Fatalf(fmt.Sprintf("[%s] %s", execName, format), v...)
}

func logWithPrefix(format string, v ...interface{}) {
	log.Printf(fmt.Sprintf("[%s] %s", execName, format), v...)
}

func bootstrapInit() {
	src := "/init"
	dst := "/oci-init"

	mountAll()
	configureNetwork(readCmdline())
	logWithPrefix("Renaming %s to %s to avoid potential naming conflict with rootfs init", src, dst)
	if err := os.Rename(src, dst); err != nil {
		logFatalWithPrefix("Failed to rename %s to %s: %v", src, dst, err)
	}
	logWithPrefix("Executing %s", dst)
	if err := syscall.Exec(dst, []string{dst}, os.Environ()); err != nil {
		logFatalWithPrefix("Exec %s failed: %v", dst, err)
	}
}

func lsRoot() {
	// Log the content of / to check ramdisk after rootfs extraction
	logWithPrefix("Listing / directory content after rootfs extraction:")
	if entries, err := os.ReadDir("/"); err == nil {
		for _, entry := range entries {
			info, _ := entry.Info()
			logWithPrefix("/%s %s %d", entry.Name(),
				func() string {
					if entry.IsDir() {
						return "dir"
					} else {
						return "file"
					}
				}(),
				info.Size())
		}
	} else {
		logWithPrefix("Failed to list /: %v", err)
	}
}

func bootstrapInitOCI() {
	params := readCmdline()
	fetchModloopImage(params)
	fetchRootfsImage(params)

	//lsRoot()

	// Mount modloop
	mountAndSymlinkModules()
	execSystemInit()
}

func execSystemInit() {
	// Exec real init
	altInits := []string{
		"/init",
		"/sbin/init",
		"/bin/init",
		"/usr/sbin/init",
		"/usr/bin/init",
	}
	for _, alt := range altInits {
		fi, err := os.Stat(alt)
		if err != nil {
			logWithPrefix("Searching for Init candidate: %s does not exist", alt)
			continue
		}
		mode := fi.Mode()
		execFlag := (mode&0111 != 0)
		logWithPrefix("Searching for Init candidate: %s exists, mode: %v, executable: %v", alt, mode, execFlag)
		if mode.IsRegular() && execFlag {
			logWithPrefix("Trying exec: %s", alt)
			if err := syscall.Exec(alt, []string{alt}, os.Environ()); err != nil {
				logWithPrefix("Exec %s failed: %v", alt, err)
			}
		}
	}
	// Si aucun init trouvé, tente /bin/sh
	if fi, err := os.Stat("/bin/sh"); err == nil && fi.Mode().IsRegular() && (fi.Mode()&0111 != 0) {
		logWithPrefix("No init found, launching /bin/sh")
		syscall.Exec("/bin/sh", []string{"/bin/sh"}, os.Environ())
	}
	logFatalWithPrefix("No executable init found, aborting")
}

// Helper for robust tty/console detection
func getConsoleWriter() *os.File {
	var console *os.File
	var err error
	for _, dev := range []string{"/dev/ttyAMA0", "/dev/ttyS0", "/dev/console"} {
		console, err = os.OpenFile(dev, os.O_WRONLY, 0)
		if err == nil {
			return console
		}
		log.Printf("[init] Warning: cannot open %s for progress bars: %v", dev, err)
	}
	log.Printf("[init] Warning: no serial/console found, falling back to os.Stdout for progress bars")
	return os.Stdout
}

func mount(mountpoint string, fsType string) {
	os.MkdirAll(mountpoint, 0755)
	if err := syscall.Mount(fsType, mountpoint, fsType, 0, ""); err != nil {
		logFatalWithPrefix("Failed to mount %s: %v", mountpoint, err)
	} else {
		logWithPrefix("Mounted %s successfully", mountpoint)
	}
}

func mountAll() {
	mount(MOUNTPOINT_FS_PROC, "proc")
	mount(MOUNTPOINT_FS_SYS, "sysfs")
	mount(MOUNTPOINT_FS_DEV, "devtmpfs")

	// Log mount options for /
	if mounts, err := os.ReadFile("/proc/mounts"); err == nil {
		for _, line := range strings.Split(string(mounts), "\n") {
			if strings.Contains(line, " / ") {
				logWithPrefix("/ mount: %s", line)
			}
		}
	} else {
		logWithPrefix("Failed to read /proc/mounts: %v", err)
	}

}

func parseNetworkParams(network string) (string, string, string, string) {
	// Format attendu: ip:netmask:gateway:dns
	parts := strings.Split(network, ":")
	if len(parts) < 2 {
		logFatalWithPrefix("Invalid network parameter: %s", network)
	}
	ip := parts[0]
	mask := ""
	gw := ""
	dns := ""
	if len(parts) > 1 {
		mask = parts[1]
	}
	if len(parts) > 2 {
		gw = parts[2]
	}
	if len(parts) > 3 {
		dns = parts[3]
	}
	if ip == "" || mask == "" {
		logFatalWithPrefix("IP or netmask missing in network parameter: %s", network)
	}
	return ip, mask, gw, dns
}

func configureNetwork(params map[string]string) {
	network, ok := params["network"]
	if !ok || network == "" {
		logFatalWithPrefix("Missing mandatory 'network' parameter in cmdline")
	}
	ip, mask, gw, dns := parseNetworkParams(network)
	networkSetIP(ip, mask)
	networkSetGW(gw)
	networkSetDNS(dns)
}

func networkSetIP(
	ip string,
	mask string,
) {
	// Compose CIDR
	cidr := ip
	if !strings.Contains(ip, "/") && mask != "" {
		// Convert netmask to prefix length
		prefix := netmaskToPrefix(mask)
		if prefix < 0 {
			logFatalWithPrefix("Invalid netmask: %s", mask)
		}
		cidr = fmt.Sprintf("%s/%d", ip, prefix)
	}
	link, err := netlink.LinkByName("eth0")
	if err != nil {
		logFatalWithPrefix("No eth0: %v", err)
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		logFatalWithPrefix("Invalid IP/CIDR: %v", err)
	}
	netlink.AddrAdd(link, addr)
	netlink.LinkSetUp(link)
}

func networkSetGW(gw string) {
	if gw != "" {
		gwIP := net.ParseIP(gw)
		route := &netlink.Route{
			Gw: gwIP,
		}
		netlink.RouteAdd(route)
	}
}

func networkSetDNS(dns string) {
	if dns != "" {
		resolv := fmt.Sprintf("nameserver %s\n", dns)
		os.WriteFile("/etc/resolv.conf", []byte(resolv), 0644)
	}
}

// Convertit un netmask (ex: 255.255.255.0) en prefix length (ex: 24) via la lib standard
func netmaskToPrefix(mask string) int {
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return -1
	}
	prefix, _ := net.IPv4Mask(ip[0], ip[1], ip[2], ip[3]).Size()
	return prefix
}

// fetchModloopImage télécharge l'image modloop et l'écrit dans /rootfs/modloop.squashfs
func fetchModloopImage(params map[string]string) {
	modloop := params[PARAM_MODLOOP]
	ociHttp := params[PARAM_OCI_HTTP]
	modloopRef := normalizeURI(modloop, SCHEME_OCI)
	logWithPrefix("Downloading modloop %s...", modloopRef)
	blob, _ := fetchBlob(modloopRef, ociHttp)
	file, err := os.Create("/modloop.squashfs")
	if err != nil {
		logFatalWithPrefix("Failed to create modloop.squashfs: %v", err)
	}
	written, err := io.Copy(file, blob.Reader)
	if err != nil {
		logFatalWithPrefix("Error downloading modloop: %v", err)
	}
	// Ajout d'un retour à la ligne après la barre de progression
	fmt.Println()
	logWithPrefix("modloop downloaded: %d bytes", written)
	file.Close()
	blob.Close()
}

func fetchRootfsImage(params map[string]string) {
	rootfs := params[PARAM_ROOTFS]
	ociHttp := params[PARAM_OCI_HTTP]
	rootfsRef := normalizeURI(rootfs, SCHEME_OCI)
	logWithPrefix("Downloading rootfs %s...", rootfsRef)
	blob, size := fetchBlob(rootfsRef, ociHttp)
	fetchAndExtractRootfs(blob.Reader, size, rootfsRef, ociHttp)
	blob.Close()
}

func parseCmdline(cmdline string) map[string]string {
	params := make(map[string]string)
	for _, param := range strings.Fields(cmdline) {
		if kv := strings.SplitN(param, "=", 2); len(kv) == 2 {
			params[kv[0]] = kv[1]
		}
	}
	return params
}

func normalizeURI(uri string, scheme string) string {
	if !strings.HasPrefix(uri, scheme) {
		logFatalWithPrefix("Unsupported URI, expected scheme: %s", scheme)
	}
	return strings.TrimPrefix(uri, scheme)
}

func fetchAndExtractRootfs(blob io.Reader, totalSize int64, rootfs string, ociHttp string) {
	bar := progressbar.NewOptions64(totalSize,
		progressbar.OptionSetDescription("Extracting rootfs "),
		progressbar.OptionSetWidth(40),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	// Peek first 6 bytes to detect format
	tee := io.TeeReader(blob, bar)
	header := make([]byte, 6)
	n, err := io.ReadFull(tee, header)
	if err != nil {
		logFatalWithPrefix("Failed to read archive header: %v", err)
	}
	var tarReader *tar.Reader
	var closer func() error = func() error { return nil }
	switch {
	case n >= 2 && header[0] == 0x1f && header[1] == 0x8b:
		// gzip
		fullReader := io.MultiReader(bytes.NewReader(header[:n]), tee)
		gzr, err := gzip.NewReader(fullReader)
		if err != nil {
			logFatalWithPrefix("Failed to open gzip rootfs: %v", err)
		}
		defer gzr.Close()
		tarReader = tar.NewReader(gzr)
		closer = gzr.Close
	case n >= 6 && header[0] == 0xfd && header[1] == '7' && header[2] == 'z' && header[3] == 'X' && header[4] == 'Z' && header[5] == 0x00:
		// xz
		fullReader := io.MultiReader(bytes.NewReader(header[:n]), tee)
		xzr, err := xz.NewReader(fullReader)
		if err != nil {
			logFatalWithPrefix("Failed to open xz rootfs: %v", err)
		}
		tarReader = tar.NewReader(xzr)
	default:
		logFatalWithPrefix("Unknown archive format (not gzip or xz)")
	}
	defer closer()
	tr := tarReader

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			logFatalWithPrefix("Error reading tar rootfs: %v", err)
		}
		path := "/" + hdr.Name
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(path, os.FileMode(hdr.Mode))
		case tar.TypeReg:
			os.MkdirAll(dirname(path), 0755)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				logFatalWithPrefix("Failed to create rootfs file: %v", err)
			}
			buf := make([]byte, 32*1024)
			for {
				n, err := tr.Read(buf)
				if n > 0 {
					if _, werr := f.Write(buf[:n]); werr != nil {
						logFatalWithPrefix("Failed to write rootfs file: %v", werr)
					}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					logFatalWithPrefix("Error reading tar file data: %v", err)
				}
			}
			f.Close()
		case tar.TypeSymlink:
			os.MkdirAll(dirname(path), 0755)
			os.Symlink(hdr.Linkname, path)
		}
	}
	// Ensure progress bar output ends with a newline
	bar.Finish()
	fmt.Println()
}

func dirname(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

func mountAndSymlinkModules() {
	modloopPath := "/modloop.squashfs"
	if _, err := os.Stat(modloopPath); err == nil {
		// Détecte le chemin du kernel
		uname := syscall.Utsname{}
		syscall.Uname(&uname)
		var release string
		for _, c := range uname.Release {
			if c == 0 {
				break
			}
			release += string(byte(c))
		}
		modulesPath := "/lib/modules/" + release
		os.MkdirAll(modulesPath, 0755)
		loopdev, err := setupLoopDevice(modloopPath)
		if err != nil {
			logFatalWithPrefix("Failed to setup loop device: %v", err)
		}
		defer detachLoopDevice(loopdev)
		if err := syscall.Mount(loopdev, modulesPath, "squashfs", 0, ""); err != nil {
			logFatalWithPrefix("Failed to mount modloop: %v", err)
		}
		os.MkdirAll("/modules", 0755)
		if err := os.Symlink(modulesPath, "/modules"); err != nil && !os.IsExist(err) {
			logFatalWithPrefix("Failed to symlink modules: %v", err)
		}
	}
}

// setupLoopDevice attache modloopPath à un /dev/loopX libre via ioctl
func setupLoopDevice(imagePath string) (string, error) {
	// Cherche un /dev/loopX libre (de loop0 à loop7)
	for i := 0; i < 8; i++ {
		loopdev := fmt.Sprintf("/dev/loop%d", i)
		// Ouvre le device
		lfd, err := os.OpenFile(loopdev, os.O_RDWR, 0)
		if err != nil {
			continue // device absent ou occupé
		}
		defer lfd.Close()
		// Ouvre le fichier image
		img, err := os.OpenFile(imagePath, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		// ioctl LOOP_SET_FD
		const LOOP_SET_FD = 0x4C00
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, lfd.Fd(), LOOP_SET_FD, img.Fd())
		img.Close()
		if errno == 0 {
			return loopdev, nil
		}
		// Si déjà utilisé, on essaye le suivant
	}
	return "", fmt.Errorf("no free loop device found")
}

// detachLoopDevice détache le fichier du loop device via ioctl
func detachLoopDevice(loopdev string) {
	lfd, err := os.OpenFile(loopdev, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer lfd.Close()
	const LOOP_CLR_FD = 0x4C01
	syscall.Syscall(syscall.SYS_IOCTL, lfd.Fd(), LOOP_CLR_FD, 0)
}

// fetchBlob télécharge un blob OCI et retourne un io.Reader et sa taille
func fetchBlob(ref string, plainHttp string) (BlobStream, int64) {
	scheme := SCHEME_HTTP
	if !strings.HasPrefix(ref, SCHEME_HTTP) && !strings.HasPrefix(ref, SCHEME_HTTPS) {
		if plainHttp != "true" {
			scheme = SCHEME_HTTPS
		}
		ref = scheme + ref
	}
	// Remove scheme for parsing
	refNoScheme := ref
	if strings.HasPrefix(refNoScheme, SCHEME_HTTP) {
		refNoScheme = strings.TrimPrefix(refNoScheme, SCHEME_HTTP)
	} else if strings.HasPrefix(refNoScheme, SCHEME_HTTPS) {
		refNoScheme = strings.TrimPrefix(refNoScheme, SCHEME_HTTPS)
	}
	// Split registry and repo:tag
	slash := strings.Index(refNoScheme, "/")
	if slash == -1 {
		logFatalWithPrefix("Invalid OCI reference: %s", ref)
	}
	registry := refNoScheme[:slash]
	repoTag := refNoScheme[slash+1:]
	tag := "latest"
	repo := repoTag
	if i := strings.LastIndex(repoTag, ":"); i != -1 {
		tag = repoTag[i+1:]
		repo = repoTag[:i]
	}
	manifestUrl := fmt.Sprintf("%s%s/v2/%s/manifests/%s", scheme, registry, repo, tag)
	logWithPrefix("Fetching manifest index: %s", manifestUrl)
	req, err := http.NewRequest("GET", manifestUrl, nil)
	if err != nil {
		logFatalWithPrefix("Error creating request: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	logWithPrefix("Request headers: %v", req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logFatalWithPrefix("Error fetching OCI index manifest: %v", err)
	}
	defer resp.Body.Close()
	indexBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		logFatalWithPrefix("Error reading OCI index manifest: %v", err)
	}
	var index struct {
		Manifests []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Platform  struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		logFatalWithPrefix("Error parsing OCI index manifest: %v", err)
	}
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	osname := runtime.GOOS
	var manifestDigest string
	for _, m := range index.Manifests {
		if m.Platform.Architecture == arch && m.Platform.OS == osname {
			manifestDigest = m.Digest
			break
		}
	}
	if manifestDigest == "" {
		logFatalWithPrefix("No compatible manifest digest for %s/%s", arch, osname)
	}
	// Fetch platform manifest
	manifestUrl2 := fmt.Sprintf("%s%s/v2/%s/manifests/%s", scheme, registry, repo, manifestDigest)
	logWithPrefix("Fetching platform manifest: %s", manifestUrl2)
	req2, err := http.NewRequest("GET", manifestUrl2, nil)
	if err != nil {
		logFatalWithPrefix("Error creating request: %v", err)
	}
	req2.Header.Set("Accept", OCI_IMAGE_MANIFEST_MEDIA_TYPE)
	logWithPrefix("Request headers: %v", req2.Header)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		logFatalWithPrefix("Error fetching OCI platform manifest: %v", err)
	}
	defer resp2.Body.Close()
	manifestBytes, err := io.ReadAll(resp2.Body)
	if err != nil {
		logFatalWithPrefix("Error reading OCI platform manifest: %v", err)
	}
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		logFatalWithPrefix("Error parsing OCI platform manifest: %v", err)
	}
	if len(manifest.Layers) == 0 {
		logFatalWithPrefix("No layers found in platform manifest")
	}
	layerDigest := manifest.Layers[0].Digest
	blobUrl := fmt.Sprintf("%s%s/v2/%s/blobs/%s", scheme, registry, repo, layerDigest)
	logWithPrefix("Fetching blob (layer): %s", blobUrl)
	blobResp, err := http.Get(blobUrl)
	if err != nil {
		logFatalWithPrefix("Error fetching OCI blob: %v", err)
	}
	blobSize := blobResp.ContentLength
	bar := progressbar.NewOptions64(blobSize,
		progressbar.OptionSetDescription("Downloading blob  "),
		progressbar.OptionSetWidth(40),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	return BlobStream{io.TeeReader(blobResp.Body, bar), blobResp.Body.Close}, blobSize
}


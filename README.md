# oci-initrd

Pure-Go initramfs `/init` (PID 1) that boots a machine by pulling its
**kernel modules** and **root filesystem** from an **OCI registry** over
HTTP, extracting them into the ramdisk, and handing off to the real init.

No shell, no busybox: a single static Go binary drives the whole early
boot.

## How it works

The binary is installed as `/init` inside the initramfs. On start it:

1. Mounts `/proc`, `/sys` and `/dev`.
2. Configures the network from the kernel command line (via `netlink`).
3. To avoid a name clash with the root filesystem's own `/init`, renames
   itself to `/oci-init` and re-executes.
4. Downloads the **modloop** image (kernel modules) to
   `/modloop.squashfs` and mounts it.
5. Downloads the **rootfs** image and extracts its tar stream
   (`gzip`- or `xz`-compressed) into the ramdisk.
6. Boots into the extracted root filesystem.

Blobs are fetched from the registry with a progress bar
(`schollz/progressbar`).

## Kernel command line

The boot target is selected entirely through `/proc/cmdline`:

| Parameter  | Meaning                                                        |
| ---------- | -------------------------------------------------------------- |
| `oci_http` | Base HTTP endpoint of the OCI registry to pull blobs from.     |
| `modloop`  | OCI reference of the kernel-modules (`.squashfs`) image.       |
| `rootfs`   | OCI reference of the root filesystem (tar, `gzip` or `xz`).    |

References accept `http://`, `https://` and `oci://` schemes.

## Build

Uses [Task](https://taskfile.dev). The default architecture is `arm64`.

```sh
task build              # -> out/initrd-oci-arm64.gz
task build ARCH=amd64   # -> out/initrd-oci-amd64.gz
task clean
task help               # list tasks
```

The Go module is named `oci-init`; the source lives in `src/init.go`.

## Relationship to the rest of nano-container-linux

`oci-initrd` produces the initramfs that fetches its payload from an OCI
registry; its sibling [`oci-pxe`](https://github.com/nano-container-linux/oci-pxe)
covers the PXE side of the same "boot from an OCI registry" idea.

## License

BSD-3-Clause — see [LICENSE](LICENSE).

# Ceraa

Async-first Go coding agent with a durable Telegram bridge. This repository is a
**binary distribution only**. No source code is published here.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh
```

The script detects your OS and architecture, downloads the matching binary,
verifies it against `SHA256SUMS`, and installs it to `/usr/local/bin` (or
`~/.local/bin` if that is not writable).

Pin a specific release:

```sh
curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh -s -- --version v0.1.0
```

Check a download without installing it:

```sh
curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh -s -- --verify-only
```

Requires `curl`, `tar` and a POSIX shell. Builds are published for `linux` and
`darwin` on `amd64` and `arm64`.

## Use

```sh
ceraa chat          # interactive session in the terminal
ceraa ask "..."     # one prompt, clean output
ceraa setup         # configure the Telegram bridge
ceraa doctor        # check config, provider, storage
ceraa version
```

## About this repository

A Go binary is a self-contained executable, so installing Ceraa is a file
download and never required the source. This repository therefore holds only
`install.sh`, the prebuilt binaries, their checksums, and the licence. The source
is developed privately and is not part of any release here.

Released binaries are built with `-trimpath`, so they carry no absolute build
paths, and the system prompt is read from disk at runtime rather than embedded.
As with any Go build, function names remain in the binary: the Go runtime needs
`.gopclntab` to build panic tracebacks and walk stacks for the garbage
collector, and no linker flag can remove that.

## Licence

Apache-2.0. See `LICENSE` and `NOTICE`.

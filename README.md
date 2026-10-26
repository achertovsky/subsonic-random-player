# Subsonic Random Player

## only author note

completely llm-generated. i do not take credit and wont fix a thing. it exist for personal usage

## Player

A small Linux terminal music player written in Go. It asks a Subsonic-compatible server (such as Navidrome) for a random song and streams it through `mpv`.

## Requirements

- Go 1.22 or newer
- `mpv`
- Access to a Subsonic-compatible server

On Debian/Ubuntu:

```sh
sudo apt update
sudo apt install golang-go mpv
```

Check the Go version with `go version`. If your distribution ships an older Go release, install a current version from [go.dev/dl](https://go.dev/dl/).

## Configure

Create the configuration directory and file:

```sh
mkdir -p ~/.config
$EDITOR ~/.config/subsonic.yaml
```

Put this in `~/.config/subsonic.yaml` and replace the example values:

```yaml
url: "https://music.example.com"
username: "your-username"
password: "your-password"
```

`url` is the base URL of your server; do not add `/rest` at the end. For example, if the API endpoint is `https://music.example.com/rest/ping.view`, use `https://music.example.com`.

Protect the file because it contains your password:

```sh
chmod 600 ~/.config/subsonic.yaml
```

Use HTTPS when connecting over an untrusted network.

## Compile

Download or clone this project, then run these commands from its directory:

```sh
go mod tidy
go build -o subsonic-random-player .
```

This creates the executable `subsonic-random-player` in the current directory. To install it into your user's local binary directory:

```sh
mkdir -p ~/.local/bin
install -m 755 subsonic-random-player ~/.local/bin/
```

Make sure `~/.local/bin` is in your `PATH`. Then run it from any directory:

```sh
subsonic-random-player
```

Alternatively, run without creating a binary:

```sh
go run .
```

## Controls

- `n` — play another random track
- `Space` — pause or resume
- `+` / `-` — raise or lower volume
- `q` — quit

## Current limitations

This is a starter version: it selects one random song on startup and lets you request another manually. Automatic next-track playback and a persistent queue are not implemented yet. Playback is handled by `mpv` over its Unix socket IPC interface.

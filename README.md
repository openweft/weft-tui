# weft-tui

Terminal UI for the [weft](https://github.com/openweft/weft) cluster
orchestrator. Single-binary `weft-tui` that connects to a running
`weft agent` over its Unix-socket gRPC and presents an interactive
view of the cluster : hosts, VMs, projects, live events.

Sibling of the CLI (`weft <noun> <verb>`) and the WebUI
([weft-webui](https://github.com/openweft/weft-webui)). Same data,
different surface — pick the one that fits the moment.

## Status

V0.1 scaffold. Hosts view functional ; VMs / Projects / Events tabs
in flight.

## Stack

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI runtime
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — styling
- [Bubbles](https://github.com/charmbracelet/bubbles) — table + viewport widgets
- weft's gRPC client over Unix socket (the same one `weft` CLI uses)

## Build

```sh
go build -o weft-tui .
```

## Run

```sh
./weft-tui                                    # uses $HOME/.weft/weft.sock
./weft-tui --socket /custom/path/weft.sock
```

## Keybindings (V0.1)

| key | action |
|---|---|
| `1` … `4` | switch tab (Hosts \| VMs \| Projects \| Events) |
| `r` | refresh active view |
| `c` | cordon selected host |
| `u` | uncordon selected host |
| `d` | set-state down (drain prep before remove) |
| `x` | remove selected host (with confirm) |
| `?` | help overlay |
| `q` / Ctrl+C | quit |

## License

BSD 3-Clause. See [LICENSE](./LICENSE).

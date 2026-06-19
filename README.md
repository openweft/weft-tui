# weft-tui

Terminal UI for the [weft](https://github.com/openweft/weft) cluster
orchestrator. Single-binary `weft-tui` that connects to a running
`weft agent` over its Unix-socket gRPC and presents an interactive
view of the cluster : hosts, VMs, projects, live events.

Sibling of the CLI (`weft <noun> <verb>`) and the WebUI
([weft-webui](https://github.com/openweft/weft-webui)). Same data,
different surface — pick the one that fits the moment.

## Status

V0.2 ships all 4 tabs (Hosts / VMs / Projects / Events) fully
functional. Future polish : command palette, themes.

## Stack

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI runtime
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — styling
- [Bubbles](https://github.com/charmbracelet/bubbles) — table, viewport, textinput widgets
- weft's gRPC client over Unix socket (the same one `weft` CLI uses)

## Build

```sh
go build -o weft-tui .
```

## Run

```sh
./weft-tui                                    # uses $HOME/.weft/weft.sock
./weft-tui --socket /custom/path/weft.sock
./weft-tui --ssh-key ~/.ssh/weft_id_ed25519   # SSH transport
```

## Keybindings (V0.2)

### Global

| key | action |
|---|---|
| `1` … `4` | switch tab (Hosts / VMs / Projects / Events) |
| `r` | refresh active tab |
| `?` | help overlay |
| `q` / Ctrl+C | quit |

### Hosts tab

| key | action |
|---|---|
| ↑/↓ or j/k | move selection |
| `c` | cordon selected host |
| `u` | uncordon selected host |
| `d` | set-state down (drain prep before remove) |
| `x` | remove selected host (with confirm) |

### VMs tab

| key | action |
|---|---|
| ↑/↓ or j/k | move selection |
| `s` | start selected VM |
| `S` | stop selected VM (with confirm) |
| `R` | restart (stop → start, sequential) |
| `l` | open serial log viewer (tail ~200 lines) |
| Esc | close log viewer / cancel confirm |

### Projects tab

| key | action |
|---|---|
| ↑/↓ or j/k | move selection |
| `n` | create new project (inline form) |
| `D` | delete selected project (with confirm) |
| Enter / Esc | submit / cancel form |

### Events tab

Live tail of cluster events. Stream opens on first visit to the tab
and stays connected across switches.

| key | action |
|---|---|
| `p` | pause / resume the stream |
| `c` | clear the buffer |
| j/k / arrows | scroll |
| PgUp / PgDn | page up / down |
| `g` / `G` | jump to top / bottom |

Component badges colour-code each line : `host` (blue) · `vm` (green) ·
`project` (yellow) · `guest` (grey) · `error` / `failed` lines redden.

## License

BSD 3-Clause. See [LICENSE](./LICENSE).

# Project presentation

[Open the presentation](https://michael-bill.github.io/knotra/).

Nine Russian slides introduce Knotra in about five minutes: process graphs, a customer-support
scenario, multi-host architecture, the verified game repair and the project. Speaker notes contain
timings and evidence boundaries. The support example needs separately connected MCP/helpdesk APIs;
the daily workload is illustrative, without measured throughput.

Open `index.html` directly or serve this directory with a static HTTP server. Assets and the
playable game are local; no engine, build step or internet connection is required.

- Arrow keys, Page Up / Page Down, Space: switch slides.
- Home / End: first / last slide.
- **H** / **Hide**: toggle controls; the eye button restores them.
- **N**: speaker notes on the same screen.
- **F**: fullscreen when supported.

Animations respect reduced motion and are disabled when printing. `Текст выступления.txt` contains
the script; `Как открыть.txt` contains Russian instructions and evidence notes. GitHub Pages deploys
this directory from `main` through `.github/workflows/pages.yml`.

Sources: [README](../../README.md), [architecture](../architecture.md) and
[recorded checks](../verification.md). Screenshots show recorded runs; the three-worker test used
one host and fixed HTTP model responses. Game counts describe the 5 October 2026 repair run, without
promising a success rate or guaranteed repair.

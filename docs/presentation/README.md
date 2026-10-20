# Project presentation

[Open the presentation](https://michael-bill.github.io/knotra/).

Nine slides in Russian: a five-minute project introduction and an extra slide about GitHub
Community. The layout uses Knotra’s light palette and logo. Slide 2 explains the use case alongside
an interactive agent such as Codex. Slide 4 highlights the local `qwen3.5:9b` model and automated
test-feedback loop; the recorded repair run starts from a deliberately incorrect strategy. A
separate generation run also passed. The presentation does not claim a measured success rate for a
single prompt or guaranteed repair. Slide 5 illustrates a seven-step customer support process: input
validation, agent analysis using customer/order/history MCP tools, another agent using knowledge and
policy tools to draft a reply, code checks, routing, optional human approval and delivery. Both
agents have read access; the final tool step sends the answer. Typical answers bypass human
approval; complex cases and failed checks go to an operator. This is an application scenario, not a
recorded run or a bundled helpdesk integration; the MCP servers and helpdesk API would need to be
connected separately. Slide 7 separates the target multi-host architecture and example daily
workload from the verified same-host worker test.

Open `index.html` directly or serve this directory with any static HTTP server. All assets and the
playable game are local; no engine, build step or internet connection is required.

- Arrow keys, Page Up / Page Down, Space: switch slides.
- Home / End: first / last slide.
- **H** or **Hide**: hide or restore the bottom controls. The eye button also restores them.
- **N**: speaker notes with planned timings, shown on the same screen.
- **F**: browser fullscreen, when supported.

`Текст выступления.txt` contains the complete script. `Как открыть.txt` contains the Russian
instructions, timing and evidence notes.

GitHub Pages deploys this directory from `main` through `.github/workflows/pages.yml`. The
screenshots come from the project’s recorded runs and the supplied Telegram post; the three-worker
results describe a same-host test with fixed model responses.

The narrative and claims are grounded in the root README, `docs/architecture.md`,
`docs/verification.md`, and `docs/scheduler-river-implementation.md`. Slide 5 uses the engine’s
`agent`, `tool`, `code`, `switch` and `human` node types to explain a possible application.

Editorial references:
[Slide Deck Builder](https://github.com/SkillMedev/skills/blob/main/skills/slide-deck-builder/SKILL.md)
and
[Presentation Design](https://github.com/rhuss/cc-slidev/blob/main/slidev/skills/presentation-design/SKILL.md).
They informed the use of declarative headlines and concrete examples.
[Official Codex documentation](https://developers.openai.com/blog/codex-as-a-platform) confirms that
Codex also supports automation; the presentation compares intended usage rather than claiming it
lacks workflow capabilities.

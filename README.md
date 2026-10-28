# orbit-forge

A plugin for [orbit](https://github.com/JGoterris/orbit): the merge requests and the CI of your repositories, in orbit,
with [`gh`](https://cli.github.com) for GitHub and [`glab`](https://gitlab.com/gitlab-org/cli) for GitLab.

> **Alpha.** It is young: it needs orbit 1.6 or later, GitLab has only been tested against a fake `glab`, and keys,
> settings and behavior may change between versions.

It adds two tabs, **MR** and **CI**, that follow the service you select (the repository of its folder), and:

- a **badge** on every service with the merge request of its branch (`#41 ✓`), or the state of its CI when it has none;
- `CI ✖ 2` in the header when CI is failing on the branches of your services, and a message when a run finishes;
- a block in the detail of each service with its merge request.

**MR** lists the open merge requests with their checks, review and size (`+120 −30`). The detail has all of one:
state, branches, labels, milestone, reviewers and what each said, the checks (the failing ones first), the description
(markdown, and the HTML that bots write), the files it changes, its commits and its conversation.

**CI** draws each run as GitHub and GitLab do: its jobs in columns, each joined to the jobs it needs, with their
state and how long they took.

```text
✓ build 1m02s ───┬─▶ ✓ test (linux) 2m10s ─┬─▶ ⊘ deploy
✓ lint    22s ─┐ ├─▶ ✖ test (macos) 2m55s ─┤
               │ └─▶ ✓ docs                │
               └───▶ ──────────────────────┘
```

On GitHub what a job needs is read from the workflow file, as it was at the commit of the run (matrices and reusable
workflows included); on GitLab from the `needs:` of the pipeline, else from its stages. In a narrow pane the jobs go
one under the other, each saying what it needs. Under it, **Failures** has the steps of the jobs that failed or are
running and the end of the output of the failed ones.

`enter` opens the selected merge request or run full screen; `[` and `]` go to the previous and next one without
leaving, `esc` goes back. Nothing is cut: long titles, values and lines are wrapped.

## Install

```bash
orbit plugin install JGoterris/orbit-forge
```

You need `gh` (and `gh auth login`) or `glab` (and `glab auth login`) for the forges you use. A repository on
`github.com` goes through `gh`; any other host is taken for a GitLab and goes through `glab`. `ORBIT_FORGE=gh|glab`
overrides that, for a GitHub Enterprise host, say. The remote is the one the branch tracks, else `origin`, else the
first one.

## Keys

| Tab | Key | |
| --- | --- | --- |
| MR | `o` · `c` · `a` · `M` · `x` | open in the browser · check out the branch · approve · merge (asks how) · close |
| MR | `m` · `d` | comment · switch between draft and ready for review |
| CI | `o` · `b` · `R` · `C` | open in the browser · this branch / all branches · run again (only the failed jobs of a failed run) · cancel |
| CI | `s` | run a workflow (the pipeline on GitLab) on a branch, now or later |
| both | `enter` · `[` `]` · `esc` | full screen · previous / next · back |

The keys of the list also work from the details of what it has selected. Everything that changes something asks
first. `tab` moves between the list and its details, `/` filters, `ctrl+r`
reloads. You can change the keys in `config.json` (`"keys": { "forge.mr-merge": "ctrl+m" }`).

On the command line, in a repository: `orbit forge mr` and `orbit forge ci [--all]`.

## Run CI later

`s` asks for the workflow, the branch and when: `now`, a delay (`30m`, `1h30m`), a time (`15:30`, the next one to
come), `tomorrow 9:00` or a date (`2026-11-02 09:00`). A run for later shows at the top of the list with `⏲` until it
starts; `C` on it cancels it. On GitHub the workflow must have `workflow_dispatch:` in its `on:`.

The schedule is kept by the plugin, not by the forge: a run starts when its time comes **if orbit is open** (any orbit:
they share the list, and only one of them starts it). One whose time passed while orbit was closed is dropped, with a
message, unless it is less than 15 minutes late. For runs that must happen whether or not orbit is open, use the `cron`
of the workflow (`on: schedule`) or the pipeline schedules of GitLab.

## Settings

In `config.json` of orbit (or `plugins:` of `orbit.yaml`):

```json
{ "plugins": { "forge": { "settings": { "interval": "1m" } } } }
```

`interval` is how often the badges are refreshed (default 30s, at least 5s): one query per repository.

## Limits

- Alpha: expect rough edges, and please open an issue when you find one.
- GitLab is covered by tests with a fake `glab`, not by a real GitLab. Reads go through `glab api`.
- The list of merge requests does not say whether it is approved on GitLab until you open it.
- What a GitLab job needs comes from its GraphQL API; where that is not available, the graph follows the stages.

## Develop

```bash
make link     # builds and uses this folder as the plugin
make test
```

orbit keeps running in the background when its window closes, and the plugin with it: after `make link`, run
`orbit down` before opening orbit again, or it goes on with the plugin it had.

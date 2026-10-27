# orbit-forge

A plugin for [orbit](https://github.com/JGoterris/orbit): the merge requests and the CI of your repositories, in orbit,
with [`gh`](https://cli.github.com) for GitHub and [`glab`](https://gitlab.com/gitlab-org/cli) for GitLab.

> **Alpha.** This is the first plugin for orbit and it is young: it needs orbit 1.5 or later, GitLab has only been
> tested against a fake `glab`, and keys, settings and behavior may change between versions.

It adds two tabs, **MR** and **CI**, that follow the service you select (the repository of its folder), and:

- a **badge** on every service with the merge request of its branch (`#41 ✓`), or the state of its CI when it has none;
- `CI ✖ 2` in the header when CI is failing on the branches of your services, and a message when a run finishes;
- a block in the detail of each service with its merge request.

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
| CI | `o` · `b` · `R` · `C` | open in the browser · this branch / all branches · run again (only the failed jobs of a failed run) · cancel |

Everything that changes something asks first. `tab` moves between the list and its details, `/` filters, `ctrl+r`
reloads. You can change the keys in `config.json` (`"keys": { "forge.mr-merge": "ctrl+m" }`).

On the command line, in a repository: `orbit forge mr` and `orbit forge ci [--all]`.

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

## Develop

```bash
make link     # builds and uses this folder as the plugin
make test
```

# Policy file

The policy tells the gardener which repositories memories may refer to, how
to recognise references, and how much to ask of people. Unknown fields are
rejected.

```json
{
  "repos": [
    {"name": "taskboard", "url": "https://github.com/example/taskboard.git"},
    {"name": "private-tool", "url": "ssh://git@git.example.com/team/private-tool.git", "aliases": ["the tool"]}
  ],
  "path_prefixes": ["~/code/"],
  "host_suffixes": [".example.internal"],
  "placeholder_hosts": ["host", "service"],
  "transient_phrases": ["not yet", "no remote yet"],
  "transient_age_days": 7,
  "max_judgements_per_run": 20,
  "min_confidence": "high",
  "max_open_proposals": 10,
  "task": {
    "project": "memory-gardener",
    "section": "Backlog",
    "priority": "low",
    "answerers": ["user:example"]
  }
}
```

| Field | Meaning | Default |
| --- | --- | --- |
| `repos` | Repositories to mirror. A memory refers to one through a `repo:<name>` scope, its `<owner>/<name>` (derived from `url`), an alias, or a path under a `path_prefixes` entry | none |
| `path_prefixes` | Workspace roots whose next segment is a repository name | none |
| `host_suffixes` | Only host names with these suffixes are resolved; empty disables DNS checks | none |
| `placeholder_hosts` | First labels that mark an example, not a real host | `host`, `hostname`, `service`, `guest`, … |
| `transient_phrases` | Phrases that make a claim temporary | `not yet`, `no remote yet`, `not diagnosed`, `work in progress`, `for now`, `temporarily` |
| `transient_age_days` | Age after which a temporary claim is checked against later commits | 7 |
| `max_judgements_per_run` | Model calls per run; the rest wait | 20 |
| `min_confidence` | Lowest model confidence (`low`, `medium`, `high`) that becomes a question; weaker verdicts count as unsure | `high` |
| `max_open_proposals` | Review tasks waiting on people at once; no new ones beyond this | 10 |
| `task.answerers` | Taskboard person principals who may answer | anyone with access |

## Signals

| Signal | Strength | Raised when |
| --- | --- | --- |
| `repo_retired` | strong | The README's opening says the repository itself is retired, archived or deprecated |
| `path_missing` | strong | A cited path existed in history but is gone from the default branch |
| `host_unresolvable` | strong | A cited host no longer resolves |
| `path_changed` | medium | Commits touched a cited path after the cited commit, or after the verification date |
| `transient_claim` | medium | A temporary claim is older than `transient_age_days` and the repository has moved since |

Only memories with a medium or strong signal go to the model. Paths are
checked only when they can be tied to one repository; host paths such as
`/etc/...` are ignored.
